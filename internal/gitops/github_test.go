package gitops

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testOwner   = "acme"
	testRepo    = "config"
	testBase    = "main"
	testBaseSHA = "1111111111111111111111111111111111111111"
	testHeadSHA = "2222222222222222222222222222222222222222"
)

// putCall records one Contents-API write.
type putCall struct {
	Path    string
	Branch  string
	Content []byte
}

// fakeGitHub answers exactly the endpoints the PR flow uses.
type fakeGitHub struct {
	mu sync.Mutex

	puts        []putCall
	createdRefs []string
	prTitle     string
	prHead      string
	prBase      string
	closed      bool
	comments    []string
	merged      bool

	// checkRuns is popped one element per check-runs poll.
	checkRuns  [][]map[string]any
	tokenCalls int
}

func (f *fakeGitHub) nextCheckRuns() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.checkRuns) == 0 {
		return nil
	}
	if len(f.checkRuns) == 1 {
		return f.checkRuns[0]
	}
	next := f.checkRuns[0]
	f.checkRuns = f.checkRuns[1:]
	return next
}

func (f *fakeGitHub) handler(t *testing.T) http.Handler {
	t.Helper()
	prefix := fmt.Sprintf("/api/v3/repos/%s/%s", testOwner, testRepo)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(code int, body any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			if err := json.NewEncoder(w).Encode(body); err != nil {
				t.Errorf("encode fake response: %v", err)
			}
		}
		p := r.URL.Path

		switch {
		case strings.HasSuffix(p, "/access_tokens") && r.Method == http.MethodPost:
			f.mu.Lock()
			f.tokenCalls++
			f.mu.Unlock()
			write(http.StatusCreated, map[string]any{
				"token":      "ghs_fake",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})

		case p == prefix+"/git/ref/heads/"+testBase && r.Method == http.MethodGet:
			write(http.StatusOK, map[string]any{
				"ref":    "refs/heads/" + testBase,
				"object": map[string]any{"sha": testBaseSHA, "type": "commit"},
			})

		case p == prefix+"/git/refs" && r.Method == http.MethodPost:
			var body struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create-ref: %v", err)
			}
			if body.SHA != testBaseSHA {
				t.Errorf("branch created from %q, want the base head %q", body.SHA, testBaseSHA)
			}
			f.mu.Lock()
			f.createdRefs = append(f.createdRefs, body.Ref)
			f.mu.Unlock()
			write(http.StatusCreated, map[string]any{
				"ref":    body.Ref,
				"object": map[string]any{"sha": body.SHA, "type": "commit"},
			})

		case strings.HasPrefix(p, prefix+"/contents/") && r.Method == http.MethodGet:
			write(http.StatusOK, map[string]any{
				"type": "file", "name": filepath.Base(p), "path": strings.TrimPrefix(p, prefix+"/contents/"),
				"sha": "oldblobsha", "size": 1, "encoding": "base64",
				"content": base64.StdEncoding.EncodeToString([]byte("old")),
			})

		case strings.HasPrefix(p, prefix+"/contents/") && r.Method == http.MethodPut:
			var body struct {
				Branch  string `json:"branch"`
				Content []byte `json:"content"`
				SHA     string `json:"sha"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode contents PUT: %v", err)
			}
			f.mu.Lock()
			f.puts = append(f.puts, putCall{
				Path:    strings.TrimPrefix(p, prefix+"/contents/"),
				Branch:  body.Branch,
				Content: body.Content,
			})
			f.mu.Unlock()
			write(http.StatusOK, map[string]any{
				"content": map[string]any{"sha": "newblobsha"},
				"commit":  map[string]any{"sha": testHeadSHA},
			})

		case p == prefix+"/pulls" && r.Method == http.MethodPost:
			var body struct {
				Title string `json:"title"`
				Head  string `json:"head"`
				Base  string `json:"base"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create-pr: %v", err)
			}
			f.mu.Lock()
			f.prTitle, f.prHead, f.prBase = body.Title, body.Head, body.Base
			f.mu.Unlock()
			write(http.StatusCreated, map[string]any{
				"number": 7,
				"head":   map[string]any{"sha": testHeadSHA},
			})

		case p == prefix+"/pulls/7" && r.Method == http.MethodGet:
			f.mu.Lock()
			closed := f.closed
			f.mu.Unlock()
			state := "open"
			if closed {
				state = "closed"
			}
			write(http.StatusOK, map[string]any{
				"number": 7, "state": state,
				"head": map[string]any{"sha": testHeadSHA},
			})

		case p == prefix+"/pulls/7" && r.Method == http.MethodPatch:
			f.mu.Lock()
			f.closed = true
			f.mu.Unlock()
			write(http.StatusOK, map[string]any{"number": 7, "state": "closed"})

		case p == prefix+"/issues/7/comments" && r.Method == http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode comment: %v", err)
			}
			f.mu.Lock()
			f.comments = append(f.comments, body.Body)
			f.mu.Unlock()
			write(http.StatusCreated, map[string]any{"id": 1})

		case p == prefix+"/commits/"+testHeadSHA+"/check-runs" && r.Method == http.MethodGet:
			runs := f.nextCheckRuns()
			write(http.StatusOK, map[string]any{
				"total_count": len(runs),
				"check_runs":  runs,
			})

		case p == prefix+"/pulls/7/merge" && r.Method == http.MethodPut:
			f.mu.Lock()
			f.merged = true
			f.mu.Unlock()
			write(http.StatusOK, map[string]any{
				"sha": "3333333333333333333333333333333333333333", "merged": true,
			})

		default:
			t.Errorf("fake GitHub got an unexpected request: %s %s", r.Method, p)
			write(http.StatusNotFound, map[string]any{"message": "not found"})
		}
	})
}

func checkRun(name, status, conclusion string) map[string]any {
	return map[string]any{"name": name, "status": status, "conclusion": conclusion}
}

// writeKey puts a fresh RSA key on disk, as the ExecStartPre helper would.
func writeKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(path, pemBytes, 0o400); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

func newTestApp(t *testing.T, f *fakeGitHub) (PRClient, TokenFunc) {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)

	client, tokenFn, err := NewGitHubApp(GitHubAppConfig{
		AppID:          123,
		InstallationID: 456,
		KeyPath:        writeKey(t),
		Owner:          testOwner,
		Repo:           testRepo,
		ManifestDir:    "deploy/manifests",
		BaseBranch:     testBase,
		BaseURL:        srv.URL,
	})
	if err != nil {
		t.Fatalf("NewGitHubApp: %v", err)
	}
	return client, tokenFn
}

func TestOpenManifestPRWritesExactlyOneManifestFile(t *testing.T) {
	f := &fakeGitHub{}
	client, _ := newTestApp(t, f)

	content := []byte("name: web\n")
	pr, err := client.OpenManifestPR(t.Context(), "web", content, "deploy web", "body")
	if err != nil {
		t.Fatalf("OpenManifestPR: %v", err)
	}
	if pr != 7 {
		t.Errorf("PR number = %d, want 7", pr)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.puts) != 1 {
		t.Fatalf("wrote %d files, want exactly 1", len(f.puts))
	}
	if f.puts[0].Path != "deploy/manifests/web.yaml" {
		t.Errorf("wrote %q, want deploy/manifests/web.yaml", f.puts[0].Path)
	}
	if string(f.puts[0].Content) != string(content) {
		t.Errorf("content = %q, want %q", f.puts[0].Content, content)
	}
	if len(f.createdRefs) != 1 || !strings.HasPrefix(f.createdRefs[0], "refs/heads/snowdeploy/") {
		t.Errorf("createdRefs = %v", f.createdRefs)
	}
	if f.puts[0].Branch != strings.TrimPrefix(f.createdRefs[0], "refs/heads/") {
		t.Errorf("wrote to %q, not the created branch %q", f.puts[0].Branch, f.createdRefs[0])
	}
	if f.prBase != testBase {
		t.Errorf("PR base = %q, want %q", f.prBase, testBase)
	}
	if f.prTitle != "deploy web" {
		t.Errorf("PR title = %q", f.prTitle)
	}
}

func TestOpenManifestPRRejectsBadServiceName(t *testing.T) {
	f := &fakeGitHub{}
	client, _ := newTestApp(t, f)
	if _, err := client.OpenManifestPR(t.Context(), "../../plan/ROADMAP", []byte("x"), "t", "b"); err == nil {
		t.Fatal("traversal service name accepted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.puts) != 0 {
		t.Fatalf("a rejected name still wrote %d files", len(f.puts))
	}
}

func TestWaitChecksSuccess(t *testing.T) {
	f := &fakeGitHub{checkRuns: [][]map[string]any{{
		checkRun("verify", "completed", "success"),
		checkRun("lint", "completed", "skipped"),
	}}}
	client, _ := newTestApp(t, f)

	ok, detail, err := client.WaitChecks(t.Context(), 7, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitChecks: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, detail = %q", detail)
	}
}

func TestWaitChecksFailureNamesTheCheck(t *testing.T) {
	f := &fakeGitHub{checkRuns: [][]map[string]any{{
		checkRun("verify", "completed", "failure"),
	}}}
	client, _ := newTestApp(t, f)

	ok, detail, err := client.WaitChecks(t.Context(), 7, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitChecks: %v", err)
	}
	if ok {
		t.Fatal("a failed check reported ok")
	}
	if !strings.Contains(detail, "verify") {
		t.Errorf("detail = %q, want it to name the failed check", detail)
	}
}

func TestWaitChecksWaitsForPendingRuns(t *testing.T) {
	f := &fakeGitHub{checkRuns: [][]map[string]any{
		{},
		{checkRun("verify", "in_progress", "")},
		{checkRun("verify", "completed", "success")},
	}}
	client, _ := newTestApp(t, f)

	ok, detail, err := client.WaitChecks(t.Context(), 7, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitChecks: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false after the run completed, detail = %q", detail)
	}
}

func TestWaitChecksHonorsContext(t *testing.T) {
	f := &fakeGitHub{checkRuns: [][]map[string]any{
		{checkRun("verify", "in_progress", "")},
	}}
	client, _ := newTestApp(t, f)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	if _, _, err := client.WaitChecks(ctx, 7, time.Millisecond); err == nil {
		t.Fatal("WaitChecks ignored a cancelled context")
	}
}

func TestMergeReturnsSHA(t *testing.T) {
	f := &fakeGitHub{}
	client, _ := newTestApp(t, f)

	sha, err := client.Merge(t.Context(), 7)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if sha != "3333333333333333333333333333333333333333" {
		t.Errorf("merge sha = %q", sha)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.merged {
		t.Error("merge endpoint not called")
	}
}

func TestClosePRCommentsThenCloses(t *testing.T) {
	f := &fakeGitHub{}
	client, _ := newTestApp(t, f)

	if err := client.ClosePR(t.Context(), 7, "verify failed"); err != nil {
		t.Fatalf("ClosePR: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "verify failed") {
		t.Errorf("comments = %v", f.comments)
	}
	if !f.closed {
		t.Error("PR was not closed")
	}
}

func TestTokenFnFailsClosedWhenKeyUnavailable(t *testing.T) {
	srv := httptest.NewServer((&fakeGitHub{}).handler(t))
	defer srv.Close()

	_, tokenFn, err := NewGitHubApp(GitHubAppConfig{
		AppID:          123,
		InstallationID: 456,
		KeyPath:        filepath.Join(t.TempDir(), "absent.pem"),
		Owner:          testOwner,
		Repo:           testRepo,
		BaseBranch:     testBase,
		BaseURL:        srv.URL,
	})
	if err != nil {
		t.Fatalf("NewGitHubApp must not read the key at construction: %v", err)
	}

	_, err = tokenFn(t.Context())
	if err == nil {
		t.Fatal("tokenFn succeeded with no key on disk")
	}
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Errorf("err = %v, want ErrCredentialUnavailable", err)
	}
	if !strings.Contains(err.Error(), "sealed") {
		t.Errorf("err = %q, want it to name the sealed-store possibility", err)
	}
}

func TestTokenFnMintsPerCall(t *testing.T) {
	f := &fakeGitHub{}
	_, tokenFn := newTestApp(t, f)

	for range 2 {
		tok, err := tokenFn(t.Context())
		if err != nil {
			t.Fatalf("tokenFn: %v", err)
		}
		if tok != "ghs_fake" {
			t.Fatalf("token = %q", tok)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenCalls == 0 {
		t.Error("no installation token was minted")
	}
}

func TestNewGitHubAppRequiresIdentity(t *testing.T) {
	for _, cfg := range []GitHubAppConfig{
		{InstallationID: 1, KeyPath: "k", Owner: "o", Repo: "r"},
		{AppID: 1, KeyPath: "k", Owner: "o", Repo: "r"},
		{AppID: 1, InstallationID: 1, Owner: "o", Repo: "r"},
		{AppID: 1, InstallationID: 1, KeyPath: "k", Repo: "r"},
		{AppID: 1, InstallationID: 1, KeyPath: "k", Owner: "o"},
	} {
		if _, _, err := NewGitHubApp(cfg); err == nil {
			t.Errorf("incomplete config accepted: %+v", cfg)
		}
	}
}
