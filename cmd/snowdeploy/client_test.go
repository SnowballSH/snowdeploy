package main

import (
	"encoding/json"
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

const goodManifest = `name: web
image:
  repository: registry.example.com/acme/web
  digest: sha256:0000000000000000000000000000000000000000000000000000000000000abc
template: web.container.tmpl
port: 8080
network: acme
health:
  url: http://127.0.0.1:8080/healthz
`

const taggedManifest = `name: api
image:
  repository: registry.example.com/acme/api:latest
  digest: latest
template: web.container.tmpl
port: 8081
network: acme
health:
  url: http://127.0.0.1:8081/healthz
`

const outsidePrefixManifest = `name: db
image:
  repository: registry.example.com/acme/db
  digest: sha256:0000000000000000000000000000000000000000000000000000000000000def
template: web.container.tmpl
port: 8082
network: acme
volumes:
  - /etc:/data
health:
  url: http://127.0.0.1:8082/healthz
`

func fixtureDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

type capture struct {
	out, err strings.Builder
}

func runCLI(args ...string) (int, *capture) {
	c := &capture{}
	code := run(args, &c.out, &c.err)
	return code, c
}

// ---- validate --------------------------------------------------------------

func TestValidatePassesOnGoodManifests(t *testing.T) {
	dir := fixtureDir(t, map[string]string{"web.yaml": goodManifest})

	code, c := runCLI("validate", dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, c.out.String(), c.err.String())
	}
	if !strings.Contains(c.out.String(), "web.yaml") {
		t.Errorf("output does not name what it checked: %q", c.out.String())
	}
}

func TestValidateFailsAndNamesTheBadFile(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"web.yaml": goodManifest,
		"api.yaml": taggedManifest,
	})

	code, c := runCLI("validate", dir)
	if code == 0 {
		t.Fatal("a tag-pinned manifest passed validation")
	}
	combined := c.out.String() + c.err.String()
	if !strings.Contains(combined, "api.yaml") {
		t.Errorf("failure does not name api.yaml: %q", combined)
	}
	if strings.Contains(c.err.String(), "web.yaml") {
		t.Errorf("the good manifest was reported as failing: %q", c.err.String())
	}
}

func TestValidateSkipsPrefixChecksUnlessAsked(t *testing.T) {
	dir := fixtureDir(t, map[string]string{"db.yaml": outsidePrefixManifest})

	if code, c := runCLI("validate", dir); code != 0 {
		t.Fatalf("prefix check ran with no prefixes configured: %d %s", code, c.err.String())
	}

	code, c := runCLI("validate", "--volume-prefixes", "/srv/acme/", dir)
	if code == 0 {
		t.Fatal("a volume outside the given prefix passed")
	}
	if !strings.Contains(c.out.String()+c.err.String(), "db.yaml") {
		t.Errorf("failure does not name db.yaml: %q", c.err.String())
	}
}

func TestValidateRejectsAnEmptyDirectory(t *testing.T) {
	if code, _ := runCLI("validate", t.TempDir()); code == 0 {
		t.Fatal("an empty manifest directory passed; that is never what the operator meant")
	}
}

func TestValidateRejectsAMissingDirectory(t *testing.T) {
	if code, _ := runCLI("validate", filepath.Join(t.TempDir(), "absent")); code == 0 {
		t.Fatal("a missing directory passed")
	}
}

func TestValidateNeedsADirectory(t *testing.T) {
	if code, _ := runCLI("validate"); code == 0 {
		t.Fatal("validate with no argument passed")
	}
}

// ---- server-backed subcommands ---------------------------------------------

type fakeDaemon struct {
	mu sync.Mutex

	deployBody  map[string]string
	deployPath  string
	authSeen    string
	rollbackHit bool
	journalID   int64

	states  []string
	started chan struct{}
}

// begun marks a deploy or rollback as accepted, releasing the event stream.
func (f *fakeDaemon) begun(id int64) {
	f.mu.Lock()
	f.journalID = id
	f.mu.Unlock()
	close(f.started)
}

func (f *fakeDaemon) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/services", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authSeen = r.Header.Get("Authorization")
		f.mu.Unlock()
		writeJSON(t, w, []map[string]any{{
			"name":            "web",
			"repository":      "registry.example.com/acme/web",
			"manifestDigest":  "sha256:aaa",
			"runningDigest":   "sha256:aaa",
			"latestAvailable": "bbb",
			"drifted":         false,
			"repoWebUrl":      "https://github.com/acme/config",
			"revisions": map[string]any{
				"sha256:aaa": map[string]any{
					"sha": "abc1234", "subject": "pin the good build",
				},
			},
			"lastDeploy": map[string]any{
				"ID": 9, "Service": "web", "Action": "deploy", "Actor": "admin",
				"NewDigest": "sha256:aaa", "PRNumber": 12, "State": "checks",
				"StartedAt": time.Now().UTC(),
			},
		}})
	})

	mux.HandleFunc("GET /api/v1/services/{name}/history", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []map[string]any{{
			"ID": 1, "Service": "web", "Action": "deploy", "Actor": "admin",
			"NewDigest": "bbb", "PRNumber": 42, "MergeSHA": "deadbeef", "State": "healthy",
			"StartedAt": time.Now().UTC(), "FinishedAt": time.Now().UTC(),
		}})
	})

	mux.HandleFunc("POST /api/v1/services/{name}/deploy", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.deployBody = body
		f.deployPath = r.URL.Path
		f.mu.Unlock()
		writeJSON(t, w, map[string]int64{"journalId": 7})
		f.begun(7)
	})

	mux.HandleFunc("POST /api/v1/services/{name}/rollback", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.rollbackHit = true
		f.deployBody = body
		f.mu.Unlock()
		writeJSON(t, w, map[string]int64{"journalId": 8})
		f.begun(8)
	})

	mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		flusher.Flush()

		// Wait for the request that the events belong to, so the stream can
		// carry the journal id the client is actually following.
		select {
		case <-f.started:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			return
		}

		f.mu.Lock()
		states := append([]string(nil), f.states...)
		journalID := f.journalID
		f.mu.Unlock()

		for _, state := range states {
			payload, _ := json.Marshal(map[string]any{
				"service": "web", "state": state, "detail": state + " detail",
				"journalId": journalID, "at": time.Now().UTC(),
			})
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(2 * time.Millisecond)
		}
		<-r.Context().Done()
	})

	return mux
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func newFakeDaemon(t *testing.T, states ...string) (*fakeDaemon, string) {
	t.Helper()
	f := &fakeDaemon{states: states, started: make(chan struct{})}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func TestStatusPrintsATable(t *testing.T) {
	_, url := newFakeDaemon(t)
	code, c := runCLI("status", "--server", url)
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, c.err.String())
	}
	for _, want := range []string{"web", "bbb"} {
		if !strings.Contains(c.out.String(), want) {
			t.Errorf("status output missing %q:\n%s", want, c.out.String())
		}
	}
}

// The daemon serves a lastDeploy without a receipt and the commit subject
// behind the pinned digest; status must show both, so an operator sees a run
// in flight and a change, not a hash.
func TestStatusShowsInFlightStateAndCommitSubject(t *testing.T) {
	_, url := newFakeDaemon(t)
	code, c := runCLI("status", "--server", url)
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, c.err.String())
	}
	if !strings.Contains(c.out.String(), "deploy in flight (checks)") {
		t.Errorf("status hides the run in flight:\n%s", c.out.String())
	}
	if !strings.Contains(c.out.String(), "pin the good build") {
		t.Errorf("status output missing the commit subject:\n%s", c.out.String())
	}
}

func TestDeployPostsDigestAndFollowsToHealthy(t *testing.T) {
	f, url := newFakeDaemon(t,
		"pr-open", "checks", "merged", "reconciling", "probing", "healthy")

	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	code, c := runCLI("deploy", "web", "--digest", digest, "--server", url)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, c.out.String(), c.err.String())
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deployBody["digest"] != digest {
		t.Errorf("posted digest = %q, want %q", f.deployBody["digest"], digest)
	}
	if f.deployPath != "/api/v1/services/web/deploy" {
		t.Errorf("posted to %q", f.deployPath)
	}
	for _, want := range []string{"pr-open", "merged", "probing", "healthy"} {
		if !strings.Contains(c.out.String(), want) {
			t.Errorf("output missing state %q:\n%s", want, c.out.String())
		}
	}
}

func TestDeployExitsNonZeroOnRollback(t *testing.T) {
	_, url := newFakeDaemon(t, "pr-open", "checks", "merged", "reconciling", "rolled-back")

	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	code, c := runCLI("deploy", "web", "--digest", digest, "--server", url)
	if code == 0 {
		t.Fatalf("a rolled-back deploy exited 0:\n%s", c.out.String())
	}
	if !strings.Contains(c.out.String(), "rolled-back") {
		t.Errorf("output does not show the rollback:\n%s", c.out.String())
	}
}

func TestDeployWithoutDigestUsesLatestAvailable(t *testing.T) {
	f, url := newFakeDaemon(t, "pr-open", "healthy")

	code, c := runCLI("deploy", "web", "--server", url)
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, c.err.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deployBody["digest"] != "bbb" {
		t.Errorf("digest = %q, want the latest available", f.deployBody["digest"])
	}
}

func TestRollbackWithoutTarget(t *testing.T) {
	f, url := newFakeDaemon(t, "pr-open", "healthy")

	if code, c := runCLI("rollback", "web", "--server", url); code != 0 {
		t.Fatalf("exit = %d: %s", code, c.err.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.rollbackHit {
		t.Fatal("rollback endpoint not called")
	}
	if f.deployBody["digest"] != "" {
		t.Errorf("rollback sent a digest it was not given: %q", f.deployBody["digest"])
	}
}

func TestHistoryPrintsEntries(t *testing.T) {
	_, url := newFakeDaemon(t)
	code, c := runCLI("history", "web", "--server", url)
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, c.err.String())
	}
	if !strings.Contains(c.out.String(), "healthy") {
		t.Errorf("history output:\n%s", c.out.String())
	}
	if !strings.Contains(c.out.String(), "#42") {
		t.Errorf("history output missing the pull request number:\n%s", c.out.String())
	}
}

func TestTokenFileIsSentAsBearer(t *testing.T) {
	f, url := newFakeDaemon(t)

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("  my-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, c := runCLI("status", "--server", url, "--token-file", tokenFile); code != 0 {
		t.Fatalf("exit = %d: %s", code, c.err.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authSeen != "Bearer my-token" {
		t.Errorf("Authorization = %q, want the trimmed token", f.authSeen)
	}
}

func TestServerFlagIsRequired(t *testing.T) {
	t.Setenv("SNOWDEPLOY_SERVER", "")
	if code, _ := runCLI("status"); code == 0 {
		t.Fatal("status ran with no server address")
	}
}

func TestServerComesFromTheEnvironment(t *testing.T) {
	_, url := newFakeDaemon(t)
	t.Setenv("SNOWDEPLOY_SERVER", url)
	if code, c := runCLI("status"); code != 0 {
		t.Fatalf("exit = %d: %s", code, c.err.String())
	} else if !strings.Contains(c.out.String(), "web") {
		t.Errorf("output: %s", c.out.String())
	}
}

func TestUnknownSubcommand(t *testing.T) {
	code, c := runCLI("frobnicate")
	if code == 0 {
		t.Fatal("unknown subcommand exited 0")
	}
	if !strings.Contains(c.err.String(), "frobnicate") {
		t.Errorf("stderr = %q", c.err.String())
	}
}

func TestNoArgumentsPrintsUsage(t *testing.T) {
	code, c := runCLI()
	if code == 0 {
		t.Fatal("no arguments exited 0")
	}
	if !strings.Contains(c.err.String(), "validate") {
		t.Errorf("usage does not list the subcommands: %q", c.err.String())
	}
}

func TestVersionFlag(t *testing.T) {
	code, c := runCLI("--version")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(c.out.String(), "snowdeploy") {
		t.Errorf("stdout = %q", c.out.String())
	}
}
