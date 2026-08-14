package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"github.com/SnowballSH/snowdeploy/internal/deploy"
	"github.com/SnowballSH/snowdeploy/internal/journal"
	"github.com/SnowballSH/snowdeploy/internal/manifest"
)

const (
	digestManifest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestRunning  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	digestLatest   = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

type deployCall struct {
	Service string
	Digest  string
	Actor   string
	Action  string
}

type fakeEngine struct {
	mu    sync.Mutex
	calls []deployCall
	err   error
	drift map[string]string
	queue int64
}

func (e *fakeEngine) Deploy(_ context.Context, service, digest, actor string) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, deployCall{service, digest, actor, "deploy"})
	if e.err != nil {
		return 0, e.err
	}
	return int64(len(e.calls)), nil
}

func (e *fakeEngine) Rollback(_ context.Context, service, digest, actor string) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, deployCall{service, digest, actor, "rollback"})
	if e.err != nil {
		return 0, e.err
	}
	return int64(len(e.calls)), nil
}

func (e *fakeEngine) Drift(context.Context) (map[string]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.drift, nil
}

func (e *fakeEngine) QueueDepth() int64 { return e.queue }

func (e *fakeEngine) recorded() []deployCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]deployCall(nil), e.calls...)
}

type fakeRepo struct{}

func (fakeRepo) Sync(context.Context) (string, error) { return "head", nil }
func (fakeRepo) Services() ([]string, error)          { return []string{"web"}, nil }
func (fakeRepo) Manifest(service string) (*manifest.Manifest, []byte, error) {
	if service != "web" {
		return nil, nil, errors.New("no such service")
	}
	return &manifest.Manifest{
		Name:  "web",
		Image: manifest.Image{Repository: "registry.example.com/acme/web", Digest: digestManifest},
	}, nil, nil
}

type fakeInspector struct{}

func (fakeInspector) RunningImage(_ context.Context, service string) (string, error) {
	if service != "web" {
		return "", errors.New("not running")
	}
	return digestRunning, nil
}

type fakeWatcher struct{}

func (fakeWatcher) Latest(repository string) (string, bool) {
	if repository == "registry.example.com/acme/web" {
		return digestLatest, true
	}
	return "", false
}

type harness struct {
	srv    *httptest.Server
	engine *fakeEngine
	jrnl   *journal.Journal
	api    *Server
	token  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	j, err := journal.Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	token := "s3cr3t-cli-token"
	sum := sha256.Sum256([]byte(token))
	hashFile := filepath.Join(t.TempDir(), "token.sha256")
	if err := os.WriteFile(hashFile,
		[]byte("# operator-placed\n"+hex.EncodeToString(sum[:])+" mac\n"), 0o400); err != nil {
		t.Fatalf("write token hash: %v", err)
	}

	eng := &fakeEngine{}
	api := New(Options{
		Engine:           eng,
		Repo:             fakeRepo{},
		Inspector:        fakeInspector{},
		Watcher:          fakeWatcher{},
		History:          j,
		CLITokenHashFile: hashFile,
	})

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	return &harness{srv: srv, engine: eng, jrnl: j, api: api, token: token}
}

func (h *harness) do(t *testing.T, method, path string, body string, headers map[string]string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(t.Context(), method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func remoteUser(name string) map[string]string { return map[string]string{"Remote-User": name} }
func bearer(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

// ---- auth ------------------------------------------------------------------

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/api/v1/services",
		"/api/v1/services/web/history",
		"/api/v1/events",
	} {
		resp := h.do(t, http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, resp.StatusCode)
		}
	}
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated deploy = %d, want 401", resp.StatusCode)
	}
	if calls := h.engine.recorded(); len(calls) != 0 {
		t.Fatalf("unauthenticated request reached the engine: %v", calls)
	}
}

func TestHealthzNeedsNoIdentity(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet, "/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
}

func TestRemoteUserBecomesTheActor(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy = %d, want 202", resp.StatusCode)
	}
	calls := h.engine.recorded()
	if len(calls) != 1 || calls[0].Actor != "admin" {
		t.Fatalf("actor not attributed: %+v", calls)
	}
}

func TestCorrectBearerIsAcceptedAndLabelled(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, bearer(h.token))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy with bearer = %d, want 202", resp.StatusCode)
	}
	calls := h.engine.recorded()
	if len(calls) != 1 || calls[0].Actor != "mac" {
		t.Fatalf("bearer actor = %+v, want the token's label", calls)
	}
}

func TestWrongBearerIsRejected(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, bearer("not-the-token"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong bearer = %d, want 401", resp.StatusCode)
	}
	if calls := h.engine.recorded(); len(calls) != 0 {
		t.Fatalf("wrong bearer reached the engine: %v", calls)
	}
}

func TestMissingTokenHashFileDoesNotOpenTheDoor(t *testing.T) {
	h := newHarness(t)
	h.api.auth.tokenHashFile = filepath.Join(t.TempDir(), "gone.sha256")

	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, bearer(h.token))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer against a missing hash file = %d, want 401", resp.StatusCode)
	}
}

// ---- endpoints -------------------------------------------------------------

func TestServicesListing(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet, "/api/v1/services", "", remoteUser("admin"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /services = %d", resp.StatusCode)
	}

	var got []ServiceStatus
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("services = %+v", got)
	}
	s := got[0]
	if s.Name != "web" || s.ManifestDigest != digestManifest ||
		s.RunningDigest != digestRunning || s.LatestAvailable != digestLatest {
		t.Errorf("service status = %+v", s)
	}
}

func TestDeployRequiresADigest(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy", `{}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("deploy with no digest = %d, want 400", resp.StatusCode)
	}
}

func TestDeployPassesTheDigestThrough(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy = %d", resp.StatusCode)
	}

	var body struct {
		JournalID int64 `json:"journalId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.JournalID == 0 {
		t.Error("no journal id returned")
	}

	calls := h.engine.recorded()
	if calls[0].Service != "web" || calls[0].Digest != digestLatest {
		t.Errorf("engine call = %+v", calls[0])
	}
}

func TestRollbackWithoutDigestIsAllowed(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/rollback", `{}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("rollback = %d, want 202", resp.StatusCode)
	}
	calls := h.engine.recorded()
	if len(calls) != 1 || calls[0].Action != "rollback" || calls[0].Digest != "" {
		t.Errorf("engine call = %+v", calls)
	}
}

func TestEngineErrorSurfacesAsBadRequest(t *testing.T) {
	h := newHarness(t)
	h.engine.err = errors.New("service is already pinned to that digest")

	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("engine error = %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body["error"], "already pinned") {
		t.Errorf("error body = %v", body)
	}
}

func TestHistoryReturnsJournalEntries(t *testing.T) {
	h := newHarness(t)
	id, err := h.jrnl.Begin(journal.Entry{
		Service: "web", Action: journal.ActionDeploy, Actor: "admin",
		NewDigest: digestLatest, State: deploy.StateDetected,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.jrnl.Finish(id, journal.StateHealthy, "green"); err != nil {
		t.Fatal(err)
	}

	resp := h.do(t, http.MethodGet, "/api/v1/services/web/history?n=5", "", remoteUser("admin"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d", resp.StatusCode)
	}
	var got []journal.Entry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].State != journal.StateHealthy {
		t.Fatalf("history = %+v", got)
	}
}

func TestUnknownServiceIsNotFound(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/nope/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown service = %d, want 404", resp.StatusCode)
	}
	if calls := h.engine.recorded(); len(calls) != 0 {
		t.Fatalf("unknown service reached the engine: %v", calls)
	}
}

// ---- SSE -------------------------------------------------------------------

func TestEventsStreamDeliversEngineEvents(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Remote-User", "admin")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("connect to events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	// The stream registers before the first read returns, so publish until a
	// frame arrives rather than racing the subscription.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.api.Publish(deploy.Event{
				Service: "web", State: deploy.StateProbing,
				Detail: "probing", JournalID: 42, At: time.Now().UTC(),
			})
			time.Sleep(5 * time.Millisecond)
		}
	}()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev deploy.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode SSE frame %q: %v", line, err)
		}
		if ev.Service != "web" || ev.State != deploy.StateProbing || ev.JournalID != 42 {
			t.Fatalf("SSE event = %+v", ev)
		}
		return
	}
	t.Fatalf("no SSE data frame arrived: %v", scanner.Err())
}

func TestPublishWithNoSubscribersDoesNotBlock(t *testing.T) {
	h := newHarness(t)
	done := make(chan struct{})
	go func() {
		for range 100 {
			h.api.Publish(deploy.Event{Service: "web", State: deploy.StateChecks})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked with no subscribers")
	}
}

// ---- metrics ---------------------------------------------------------------

func TestMetricsRecordOutcomes(t *testing.T) {
	h := newHarness(t)

	h.api.Publish(deploy.Event{
		Service: "web", State: deploy.StatePROpen, JournalID: 1, At: time.Now().UTC(),
	})
	h.api.Publish(deploy.Event{
		Service: "web", State: deploy.StateHealthy, JournalID: 1,
		At: time.Now().UTC().Add(time.Second),
	})
	h.api.SetDrift(map[string]string{"web": "running something else"})

	msrv := httptest.NewServer(h.api.MetricsHandler())
	defer msrv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, msrv.URL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := msrv.Client().Do(req)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf := new(strings.Builder)
	if _, err := fmt.Fprint(buf, readAll(t, resp)); err != nil {
		t.Fatal(err)
	}
	body := buf.String()

	for _, want := range []string{
		`snowdeploy_deploys_total{outcome="healthy",service="web"} 1`,
		`snowdeploy_drift{service="web"} 1`,
		"snowdeploy_state_duration_seconds",
		"snowdeploy_queue_depth",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestMetricsHandlerServesNoDeployAPI(t *testing.T) {
	h := newHarness(t)
	msrv := httptest.NewServer(h.api.MetricsHandler())
	defer msrv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		msrv.URL+"/api/v1/services/web/deploy", strings.NewReader(`{"digest":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := msrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("the metrics listener exposed the deploy API")
	}
	if calls := h.engine.recorded(); len(calls) != 0 {
		t.Fatalf("metrics listener reached the engine: %v", calls)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}
