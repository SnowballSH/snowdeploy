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
	"github.com/SnowballSH/snowdeploy/internal/registry"
	"github.com/SnowballSH/snowdeploy/internal/revision"
)

const (
	digestManifest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestRunning  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	digestLatest   = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

	testRepoWebURL = "https://github.com/acme/config"
)

type deployCall struct {
	Service string
	Digest  string
	Actor   string
	Action  string
}

type fakeEngine struct {
	mu     sync.Mutex
	calls  []deployCall
	err    error
	joined bool
	drift  map[string]string
	queue  int64
}

func (e *fakeEngine) Deploy(_ context.Context, service, digest, actor string) (int64, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, deployCall{service, digest, actor, "deploy"})
	if e.err != nil {
		return 0, false, e.err
	}
	return int64(len(e.calls)), e.joined, nil
}

func (e *fakeEngine) Rollback(_ context.Context, service, digest, actor string) (int64, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, deployCall{service, digest, actor, "rollback"})
	if e.err != nil {
		return 0, false, e.err
	}
	return int64(len(e.calls)), e.joined, nil
}

func (e *fakeEngine) Converge(_ context.Context, service, actor string) (int64, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, deployCall{service, "", actor, "converge"})
	if e.err != nil {
		return 0, false, e.err
	}
	return int64(len(e.calls)), e.joined, nil
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
func (fakeRepo) SyncState() (bool, error)             { return true, nil }
func (fakeRepo) Manifest(service string) (*manifest.Manifest, []byte, error) {
	if service != "web" {
		return nil, nil, errors.New("no such service")
	}
	return &manifest.Manifest{
		Name:  "web",
		Image: manifest.Image{Repository: "registry.example.com/acme/web", Digest: digestManifest},
	}, nil, nil
}

// errStoreSealed stands in for the vocabulary gitops reports when the deploy
// credential cannot be read. The API only forwards it; it does not interpret it.
var errStoreSealed = errors.New(
	"the configuration repository credential is unavailable, so the daemon has " +
		"no merged manifests to read: the secret store is probably sealed")

// syntheticToken is not a credential. It stands where a real one could hide —
// an operator who ever wrote a token into the remote URL would have git quote
// it back inside a transport error.
const syntheticToken = "ghs_synthetic_never_a_real_token"

// unsyncedRepo is the daemon after a reboot with the secret store still sealed:
// the mirror was never populated, so it knows nothing about any service. It
// must never be mistaken for a repository that knows the service is absent.
type unsyncedRepo struct{}

func (unsyncedRepo) Sync(context.Context) (string, error) {
	return "", fmt.Errorf("clone https://x-access-token:%s@github.com/acme/config.git: %w",
		syntheticToken, errStoreSealed)
}
func (unsyncedRepo) Services() ([]string, error) { return nil, errStoreSealed }
func (unsyncedRepo) SyncState() (bool, error)    { return false, errStoreSealed }
func (unsyncedRepo) Manifest(string) (*manifest.Manifest, []byte, error) {
	return nil, nil, errStoreSealed
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

var watcherCheckedAt = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func (fakeWatcher) LastPoll(repository string) (registry.PollStatus, bool) {
	if repository == "registry.example.com/acme/web" {
		return registry.PollStatus{At: watcherCheckedAt}, true
	}
	return registry.PollStatus{}, false
}

// fakeRevisions knows the manifest and latest digests but not the running one,
// standing in for an image whose provenance labels never got stamped.
type fakeRevisions struct{}

func (fakeRevisions) Lookup(_ context.Context, _, digest string) (revision.Revision, bool) {
	switch digest {
	case digestManifest:
		return revision.Revision{
			SHA:     "1111abc",
			URL:     "https://github.com/acme/web/commit/1111abc",
			Subject: "pin the manifest digest",
		}, true
	case digestLatest:
		return revision.Revision{
			SHA:     "3333abc",
			URL:     "https://github.com/acme/web/commit/3333abc",
			Subject: "the newest build",
		}, true
	default:
		return revision.Revision{}, false
	}
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
	return newHarnessWithSource(t, fakeRepo{}, fakeRevisions{})
}

func newHarnessWithRepo(t *testing.T, repo Repo) *harness {
	t.Helper()
	return newHarnessWithSource(t, repo, fakeRevisions{})
}

func newHarnessWithSource(t *testing.T, repo Repo, revisions revision.Source) *harness {
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
		Repo:             repo,
		Inspector:        fakeInspector{},
		Watcher:          fakeWatcher{},
		History:          j,
		CLITokenHashFile: hashFile,
		Revisions:        revisions,
		RepoWebURL:       testRepoWebURL,
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
	if !s.RegistryReachable || !s.LatestCheckedAt.Equal(watcherCheckedAt) {
		t.Errorf("registry poll status lost: reachable=%v checkedAt=%v",
			s.RegistryReachable, s.LatestCheckedAt)
	}
	if s.RepoWebURL != testRepoWebURL {
		t.Errorf("repoWebUrl = %q, want %q", s.RepoWebURL, testRepoWebURL)
	}
}

// The status row annotates only the digests something is known about: the
// running digest's image carries no labels, and inventing an empty revision
// for it would make "unlabelled" indistinguishable from "labelled as empty".
func TestServiceStatusCarriesRevisionsForKnownDigestsOnly(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet, "/api/v1/services", "", remoteUser("admin"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /services = %d", resp.StatusCode)
	}

	var got []ServiceStatus
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	revs := got[0].Revisions
	if len(revs) != 2 {
		t.Fatalf("revisions = %+v, want the manifest and latest digests only", revs)
	}
	if revs[digestManifest].SHA != "1111abc" || revs[digestManifest].Subject == "" {
		t.Errorf("manifest revision = %+v", revs[digestManifest])
	}
	if revs[digestLatest].SHA != "3333abc" {
		t.Errorf("latest revision = %+v", revs[digestLatest])
	}
	if _, present := revs[digestRunning]; present {
		t.Error("an unknown digest was given a revision entry")
	}
}

// ---- the revisions endpoint ------------------------------------------------

func TestRevisionsEndpointMapsKnownDigestsAndOmitsTheRest(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet,
		"/api/v1/services/web/revisions?digests="+digestManifest+","+digestRunning,
		"", remoteUser("admin"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /revisions = %d", resp.StatusCode)
	}

	var got map[string]revision.Revision
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("revisions = %+v, want the one known digest", got)
	}
	rev := got[digestManifest]
	if rev.SHA != "1111abc" || rev.URL == "" || rev.Subject == "" {
		t.Errorf("revision = %+v", rev)
	}
}

func TestRevisionsEndpointRequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet,
		"/api/v1/services/web/revisions?digests="+digestManifest, "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated revisions = %d, want 401", resp.StatusCode)
	}
}

func TestRevisionsEndpointValidatesItsDigests(t *testing.T) {
	h := newHarness(t)
	for name, query := range map[string]string{
		"missing param":  "",
		"not a digest":   "?digests=latest",
		"crafted digest": "?digests=sha256:" + strings.Repeat("g", 64),
		"empty entry":    "?digests=" + digestManifest + ",",
	} {
		resp := h.do(t, http.MethodGet, "/api/v1/services/web/revisions"+query,
			"", remoteUser("admin"))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestRevisionsEndpointCapsTheDigestCount(t *testing.T) {
	h := newHarness(t)

	digests := make([]string, 0, maxRevisionDigests+1)
	for range maxRevisionDigests + 1 {
		digests = append(digests, digestManifest)
	}
	resp := h.do(t, http.MethodGet,
		"/api/v1/services/web/revisions?digests="+strings.Join(digests, ","),
		"", remoteUser("admin"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%d digests = %d, want 400", maxRevisionDigests+1, resp.StatusCode)
	}

	resp = h.do(t, http.MethodGet,
		"/api/v1/services/web/revisions?digests="+strings.Join(digests[1:], ","),
		"", remoteUser("admin"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d digests = %d, want 200", maxRevisionDigests, resp.StatusCode)
	}
}

func TestRevisionsEndpointRejectsAnUnknownService(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet,
		"/api/v1/services/nope/revisions?digests="+digestManifest, "", remoteUser("admin"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown service = %d, want 404", resp.StatusCode)
	}
}

// A daemon built without a revision source must degrade to plain digests
// everywhere, never panic: the field is decoration, not a dependency.
func TestNilRevisionSourceDegradesToAbsentFields(t *testing.T) {
	h := newHarnessWithSource(t, fakeRepo{}, nil)

	resp := h.do(t, http.MethodGet, "/api/v1/services", "", remoteUser("admin"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /services = %d", resp.StatusCode)
	}
	var services []ServiceStatus
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if services[0].Revisions != nil {
		t.Errorf("revisions invented without a source: %+v", services[0].Revisions)
	}

	resp = h.do(t, http.MethodGet,
		"/api/v1/services/web/revisions?digests="+digestManifest, "", remoteUser("admin"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /revisions = %d, want 200", resp.StatusCode)
	}
	var got map[string]revision.Revision
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("revisions = %+v, want an empty map", got)
	}
}

// blockingRevisions is a cache that is cold and stays cold: every lookup
// parks until the caller's context expires and then knows nothing.
type blockingRevisions struct{}

func (blockingRevisions) Lookup(ctx context.Context, _, _ string) (revision.Revision, bool) {
	<-ctx.Done()
	return revision.Revision{}, false
}

// A cold revision cache must cost a status row one shared budget, not one
// budget per digest and not the whole fill: the listing answers with plain
// digests and the annotations appear on a later refresh.
func TestStatusRevisionLookupsShareOneBudget(t *testing.T) {
	h := newHarnessWithSource(t, fakeRepo{}, blockingRevisions{})

	start := time.Now()
	resp := h.do(t, http.MethodGet, "/api/v1/services", "", remoteUser("admin"))
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /services = %d", resp.StatusCode)
	}
	var services []ServiceStatus
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if services[0].Revisions != nil {
		t.Errorf("a cold cache produced revisions: %+v", services[0].Revisions)
	}
	if elapsed >= 2*statusRevisionBudget {
		t.Errorf("listing took %v; the row's lookups did not share one %v budget",
			elapsed, statusRevisionBudget)
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
		Joined    bool  `json:"joined"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.JournalID == 0 {
		t.Error("no journal id returned")
	}
	if body.Joined {
		t.Error("a fresh deploy claimed to have joined an existing one")
	}

	calls := h.engine.recorded()
	if calls[0].Service != "web" || calls[0].Digest != digestLatest {
		t.Errorf("engine call = %+v", calls[0])
	}
}

// A click that matched a run already carrying the same digest is answered
// with that run's id; the joined flag is how a client knows to say "joined an
// existing deploy" instead of "started one".
func TestJoinedDeployIsReportedAsJoined(t *testing.T) {
	h := newHarness(t)
	h.engine.mu.Lock()
	h.engine.joined = true
	h.engine.mu.Unlock()

	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy = %d, want 202", resp.StatusCode)
	}
	var body struct {
		JournalID int64 `json:"journalId"`
		Joined    bool  `json:"joined"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Joined {
		t.Error("the joined flag was lost between the engine and the client")
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

func TestConvergeTakesNoBodyAndDispatches(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/converge", ``, remoteUser("admin"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("converge = %d, want 202", resp.StatusCode)
	}
	var body struct {
		JournalID int64 `json:"journalId"`
		Joined    bool  `json:"joined"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.JournalID == 0 {
		t.Error("no journal id returned")
	}
	calls := h.engine.recorded()
	if len(calls) != 1 || calls[0].Action != "converge" ||
		calls[0].Service != "web" || calls[0].Digest != "" {
		t.Errorf("engine call = %+v", calls)
	}
	if calls[0].Actor != "admin" {
		t.Errorf("actor = %q", calls[0].Actor)
	}
}

func TestConvergeOnAnUnknownServiceIsNotFound(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/nope/converge", ``, remoteUser("admin"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("converge on unknown service = %d, want 404", resp.StatusCode)
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

// ---- a configuration repository that was never read ------------------------

func errorField(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body["error"]
}

// A daemon that has never read the configuration repository knows nothing
// about any service, which is not the same claim as knowing a service is
// absent. Answering 404 "no such service" sends the operator hunting for a
// deleted manifest when the actual fix is to unseal the secret store.
func TestDeployAgainstAnUnreadRepositoryIsNotAMissingService(t *testing.T) {
	h := newHarnessWithRepo(t, unsyncedRepo{})

	resp := h.do(t, http.MethodPost, "/api/v1/services/portfolio/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("deploy against an unread repository = %d, want 503", resp.StatusCode)
	}

	detail := errorField(t, resp)
	if strings.Contains(detail, "no such service") {
		t.Errorf("an unread repository claimed the service does not exist: %q", detail)
	}
	if !strings.Contains(detail, "sealed") {
		t.Errorf("error body does not say why the daemon cannot answer: %q", detail)
	}
	if calls := h.engine.recorded(); len(calls) != 0 {
		t.Fatalf("request reached the engine: %v", calls)
	}
}

// The two cases must stay tellable apart: one means "come back later", the
// other means "you asked for something that is not there".
func TestUnknownServiceStaysDistinctFromAnUnreadRepository(t *testing.T) {
	known := newHarness(t)
	resp := known.do(t, http.MethodPost, "/api/v1/services/nope/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown service on a read repository = %d, want 404", resp.StatusCode)
	}
	if detail := errorField(t, resp); !strings.Contains(detail, "no such service") {
		t.Errorf("unknown service = %q, want it to say so plainly", detail)
	}

	unread := newHarnessWithRepo(t, unsyncedRepo{})
	resp = unread.do(t, http.MethodPost, "/api/v1/services/nope/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("same name on an unread repository = %d, want 503", resp.StatusCode)
	}
}

// An empty list is a claim: "there are no services". A daemon with no copy of
// the configuration repository is not entitled to make it.
func TestServiceListSaysWhyItHasNothingToList(t *testing.T) {
	h := newHarnessWithRepo(t, unsyncedRepo{})

	resp := h.do(t, http.MethodGet, "/api/v1/services", "", remoteUser("admin"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /services on an unread repository = %d, want 503", resp.StatusCode)
	}
	if detail := errorField(t, resp); !strings.Contains(detail, "sealed") {
		t.Errorf("error body does not say why the list is missing: %q", detail)
	}
}

// The sync error itself must never reach a client: a git transport error
// quotes the remote URL, and a URL is somewhere a credential can hide.
func TestTheSyncErrorTextNeverReachesTheClient(t *testing.T) {
	h := newHarnessWithRepo(t, unsyncedRepo{})

	for _, path := range []string{"/api/v1/services", "/api/v1/services/web/history"} {
		resp := h.do(t, http.MethodGet, path, "", remoteUser("admin"))
		if detail := errorField(t, resp); strings.Contains(detail, syntheticToken) {
			t.Errorf("GET %s echoed the raw sync error to the client: %q", path, detail)
		}
	}
}

// The POST path has the same exposure as the reads: an engine error that grew
// out of a failed sync embeds whatever the Git transport said, remote URL
// included. The client gets the repository's own reason and a gateway status,
// never the error text.
func TestTheSyncErrorTextNeverReachesTheDeployClient(t *testing.T) {
	h := newHarnessWithRepo(t, staleRepo{})
	h.engine.mu.Lock()
	h.engine.err = fmt.Errorf(
		"sync configuration repository: fetch main: https://x-access-token:%s@github.com/acme/config.git: unreachable",
		syntheticToken)
	h.engine.mu.Unlock()

	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("deploy over a failing sync = %d, want 502", resp.StatusCode)
	}
	detail := errorField(t, resp)
	if strings.Contains(detail, syntheticToken) {
		t.Errorf("the raw sync error reached the client: %q", detail)
	}
	if !strings.Contains(detail, "could not be fetched") {
		t.Errorf("error body does not carry the repository's own reason: %q", detail)
	}
}

// An engine error that is not a sync failure stays a plain 400 with its own
// text: validation answers are the client's to read.
func TestValidationErrorsStillReachTheDeployClient(t *testing.T) {
	h := newHarness(t)
	h.engine.mu.Lock()
	h.engine.err = errors.New("service is already pinned to that digest")
	h.engine.mu.Unlock()

	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("validation error = %d, want 400", resp.StatusCode)
	}
	if detail := errorField(t, resp); !strings.Contains(detail, "already pinned") {
		t.Errorf("validation detail lost: %q", detail)
	}
}

// staleRepo has a populated mirror whose refresh is failing: it still knows
// every service the last successful sync merged.
type staleRepo struct{ fakeRepo }

func (staleRepo) Sync(context.Context) (string, error) {
	return "", fmt.Errorf("fetch main: https://x-access-token:%s@github.com/acme/config.git: %w",
		syntheticToken, errRemoteUnreachable)
}
func (staleRepo) SyncState() (bool, error) { return true, errRemoteUnreachable }

var errRemoteUnreachable = errors.New("the configuration repository could not be fetched")

// A mirror the daemon cannot refresh is a different failure from one it never
// had: there the daemon really is a gateway to a remote that did not answer,
// and the services it already knows stay actionable.
func TestAStaleMirrorReportsTheRemoteAndKeepsItsServices(t *testing.T) {
	h := newHarnessWithRepo(t, staleRepo{})

	resp := h.do(t, http.MethodGet, "/api/v1/services", "", remoteUser("admin"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("GET /services with a stale mirror = %d, want 502", resp.StatusCode)
	}
	detail := errorField(t, resp)
	if strings.Contains(detail, syntheticToken) {
		t.Errorf("the raw fetch error reached the client: %q", detail)
	}
	if !strings.Contains(detail, "could not be fetched") {
		t.Errorf("error body does not name the failure: %q", detail)
	}

	resp = h.do(t, http.MethodPost, "/api/v1/services/web/deploy",
		`{"digest":"`+digestLatest+`"}`, remoteUser("admin"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy of a known service during a refresh failure = %d, want 202",
			resp.StatusCode)
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

// openEventStream connects and returns a scanner over the stream plus the
// response for cleanup.
func openEventStream(t *testing.T, h *harness) *bufio.Scanner {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Remote-User", "admin")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("connect to events: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return bufio.NewScanner(resp.Body)
}

// A page opened mid-deploy used to show nothing until the next transition
// happened to arrive. The stream now opens with one synthetic frame per
// service whose newest journal row is still in flight, links included.
func TestEventsStreamSendsASnapshotOfInFlightRuns(t *testing.T) {
	h := newHarness(t)
	id, err := h.jrnl.Begin(journal.Entry{
		Service: "web", Action: journal.ActionDeploy, Actor: "admin",
		NewDigest: digestLatest, PRNumber: 12, MergeSHA: "abc123",
		State: deploy.StateChecks, Detail: "waiting for checks",
	})
	if err != nil {
		t.Fatal(err)
	}

	scanner := openEventStream(t, h)
	var lastID string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			lastID = strings.TrimPrefix(line, "id: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev deploy.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode SSE frame %q: %v", line, err)
		}
		if ev.JournalID != id || ev.State != deploy.StateChecks || ev.Service != "web" {
			t.Fatalf("snapshot frame = %+v", ev)
		}
		if ev.Action != journal.ActionDeploy {
			t.Errorf("snapshot frame lost the action: %+v", ev)
		}
		if want := testRepoWebURL + "/pull/12"; ev.PRURL != want {
			t.Errorf("snapshot pr url = %q, want %q", ev.PRURL, want)
		}
		if want := testRepoWebURL + "/commit/abc123"; ev.MergeURL != want {
			t.Errorf("snapshot merge url = %q, want %q", ev.MergeURL, want)
		}
		if want := fmt.Sprintf("%d-1", id); lastID != want {
			t.Errorf("frame id = %q, want %q", lastID, want)
		}
		return
	}
	t.Fatalf("no snapshot frame arrived: %v", scanner.Err())
}

// A finished row is history, not state: the snapshot must skip it, so the
// first frame a fresh connection sees is the next live transition.
func TestEventsStreamSkipsFinishedRunsInTheSnapshot(t *testing.T) {
	h := newHarness(t)
	id, err := h.jrnl.Begin(journal.Entry{
		Service: "web", Action: journal.ActionDeploy, Actor: "admin",
		NewDigest: digestLatest, State: deploy.StateChecks,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.jrnl.Finish(id, journal.StateHealthy, "done"); err != nil {
		t.Fatal(err)
	}

	scanner := openEventStream(t, h)
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
				Service: "web", State: deploy.StateProbing, JournalID: 42,
				At: time.Now().UTC(),
			})
			time.Sleep(5 * time.Millisecond)
		}
	}()

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev deploy.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode SSE frame %q: %v", line, err)
		}
		if ev.JournalID == id {
			t.Fatalf("a finished run was snapshot as in flight: %+v", ev)
		}
		if ev.JournalID == 42 {
			return
		}
	}
	t.Fatalf("no live frame arrived: %v", scanner.Err())
}

// A subscriber that falls behind loses events by design; the losses must add
// up somewhere an alert can read, and the log must say it once, not once per
// dropped event.
func TestDroppedEventsAreCounted(t *testing.T) {
	h := newHarness(t)

	_, unsubscribe := h.api.broker.subscribe()
	defer unsubscribe()

	const published = subscriberBuffer + 10
	for range published {
		h.api.Publish(deploy.Event{Service: "web", State: deploy.StateChecks, JournalID: 1})
	}

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

	want := fmt.Sprintf("snowdeploy_events_dropped_total %d", published-subscriberBuffer)
	if body := readAll(t, resp); !strings.Contains(body, want) {
		t.Errorf("metrics missing %q", want)
	}
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

// A registry the daemon can no longer reach produces no deploy offer, and
// before this counter existed it produced no signal either — the UI simply
// showed nothing new, forever. The outcome label is what an alert reads.
func TestMetricsCountRegistryPollOutcomes(t *testing.T) {
	h := newHarness(t)

	h.api.RecordRegistryPoll("ghcr.io/acme/web", nil)
	h.api.RecordRegistryPoll("ghcr.io/acme/web", errors.New("dial tcp: i/o timeout"))
	h.api.RecordRegistryPoll("ghcr.io/acme/api", nil)

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
	body := readAll(t, resp)

	for _, want := range []string{
		`snowdeploy_registry_polls_total{outcome="ok",repository="ghcr.io/acme/web"} 1`,
		`snowdeploy_registry_polls_total{outcome="error",repository="ghcr.io/acme/web"} 1`,
		`snowdeploy_registry_polls_total{outcome="ok",repository="ghcr.io/acme/api"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	// The error itself must never reach the exposition: a resolve failure can
	// carry a URL, and a metric label is world-readable to every scraper.
	if strings.Contains(body, "i/o timeout") {
		t.Error("the resolve error text leaked into a metric label")
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
