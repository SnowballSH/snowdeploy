package revision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSHA    = "0123456789abcdef0123456789abcdef01234567"
	testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// fakeFetcher hands out per-repository label sets and counts every call, so a
// test can tell a cache hit from a re-fetch. A non-nil gate parks every fetch
// until it is closed, letting a test hold a fill open.
type fakeFetcher struct {
	mu     sync.Mutex
	labels map[string]map[string]string
	err    error
	calls  int
	gate   chan struct{}
}

func (f *fakeFetcher) fetch(_ context.Context, repository, _ string) (map[string]string, error) {
	f.mu.Lock()
	f.calls++
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	l, ok := f.labels[repository]
	if !ok {
		return nil, errors.New("no such image")
	}
	return l, nil
}

func (f *fakeFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeGitHub answers the one commits endpoint the subject fetch uses.
type fakeGitHub struct {
	mu      sync.Mutex
	message string
	status  int
	calls   int
}

func (g *fakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.calls++
		message, status := g.message, g.status
		g.mu.Unlock()

		if r.Header.Get("Accept") != "application/vnd.github+json" {
			http.Error(w, "wrong accept header", http.StatusNotAcceptable)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"commit":{"message":%q}}`, message)
	})
}

func (g *fakeGitHub) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

type harness struct {
	src     *cachedSource
	fetcher *fakeFetcher
	github  *fakeGitHub
}

func newHarness(t *testing.T, labels map[string]string) *harness {
	t.Helper()

	gh := &fakeGitHub{message: "ship the feature\n\nlonger body"}
	srv := httptest.NewServer(gh.handler())
	t.Cleanup(srv.Close)

	fetcher := &fakeFetcher{labels: map[string]map[string]string{
		"registry.example.com/acme/web": labels,
	}}
	return &harness{
		src:     newCached(fetcher.fetch, srv.Client(), srv.URL),
		fetcher: fetcher,
		github:  gh,
	}
}

func githubLabels() map[string]string {
	return map[string]string{
		revisionLabel: testSHA,
		sourceLabel:   "https://github.com/acme/web",
	}
}

func (h *harness) lookup(t *testing.T) (Revision, bool) {
	t.Helper()
	return h.src.Lookup(t.Context(), "registry.example.com/acme/web", testDigest)
}

// ---- resolving -------------------------------------------------------------

func TestLookupResolvesSHAURLAndSubject(t *testing.T) {
	h := newHarness(t, githubLabels())

	rev, ok := h.lookup(t)
	if !ok {
		t.Fatal("Lookup = false for a fully labelled image")
	}
	if rev.SHA != testSHA {
		t.Errorf("SHA = %q, want %q", rev.SHA, testSHA)
	}
	if want := "https://github.com/acme/web/commit/" + testSHA; rev.URL != want {
		t.Errorf("URL = %q, want %q", rev.URL, want)
	}
	if rev.Subject != "ship the feature" {
		t.Errorf("Subject = %q, want the commit message's first line", rev.Subject)
	}
}

func TestMissingRevisionLabelIsUnknown(t *testing.T) {
	h := newHarness(t, map[string]string{sourceLabel: "https://github.com/acme/web"})
	if _, ok := h.lookup(t); ok {
		t.Fatal("Lookup = true with no revision label")
	}
}

func TestFetchErrorIsUnknown(t *testing.T) {
	h := newHarness(t, githubLabels())
	h.fetcher.err = errors.New("registry unreachable")
	if _, ok := h.lookup(t); ok {
		t.Fatal("Lookup = true when the registry could not be read")
	}
}

// A SHA with no source label is still an answer: the operator can search for
// the commit even when nothing can be linked.
func TestMissingSourceLabelStillReportsTheSHA(t *testing.T) {
	h := newHarness(t, map[string]string{revisionLabel: testSHA})

	rev, ok := h.lookup(t)
	if !ok || rev.SHA != testSHA {
		t.Fatalf("Lookup = %+v, %v; want the SHA alone", rev, ok)
	}
	if rev.URL != "" || rev.Subject != "" {
		t.Errorf("URL/Subject invented without a source label: %+v", rev)
	}
}

func TestNonGitHubSourceGetsAURLButNoSubject(t *testing.T) {
	h := newHarness(t, map[string]string{
		revisionLabel: testSHA,
		sourceLabel:   "https://gitlab.example.com/acme/web",
	})

	rev, ok := h.lookup(t)
	if !ok {
		t.Fatal("Lookup = false for a non-github source")
	}
	if want := "https://gitlab.example.com/acme/web/commit/" + testSHA; rev.URL != want {
		t.Errorf("URL = %q, want %q", rev.URL, want)
	}
	if rev.Subject != "" {
		t.Errorf("Subject = %q from an API that was never asked", rev.Subject)
	}
	if h.github.callCount() != 0 {
		t.Error("the GitHub API was asked about a non-github source")
	}
}

// A clone-URL spelling of the source label must not leak ".git" into the
// commit link or the API path.
func TestCloneURLSourceIsNormalised(t *testing.T) {
	h := newHarness(t, map[string]string{
		revisionLabel: testSHA,
		sourceLabel:   "https://github.com/acme/web.git",
	})

	rev, ok := h.lookup(t)
	if !ok {
		t.Fatal("Lookup = false")
	}
	if want := "https://github.com/acme/web/commit/" + testSHA; rev.URL != want {
		t.Errorf("URL = %q, want %q", rev.URL, want)
	}
	if rev.Subject != "ship the feature" {
		t.Errorf("Subject = %q, want the fetched subject", rev.Subject)
	}
}

func TestFailedSubjectFetchDegradesToSHAAndURL(t *testing.T) {
	h := newHarness(t, githubLabels())
	h.github.status = http.StatusForbidden

	rev, ok := h.lookup(t)
	if !ok || rev.SHA != testSHA || rev.URL == "" {
		t.Fatalf("Lookup = %+v, %v; want SHA and URL to survive a subject failure", rev, ok)
	}
	if rev.Subject != "" {
		t.Errorf("Subject = %q from a %d response", rev.Subject, http.StatusForbidden)
	}
}

func TestSubjectIsTruncatedTo100Runes(t *testing.T) {
	h := newHarness(t, githubLabels())
	h.github.message = strings.Repeat("é", 150) + "\nbody"

	rev, _ := h.lookup(t)
	if got := len([]rune(rev.Subject)); got != 100 {
		t.Errorf("subject length = %d runes, want 100", got)
	}
	if strings.Contains(rev.Subject, "body") {
		t.Error("subject carried more than the first line")
	}
}

// ---- caching ---------------------------------------------------------------

func TestSuccessfulLookupIsCachedForever(t *testing.T) {
	h := newHarness(t, githubLabels())
	h.src.now = func() time.Time { return time.Unix(0, 0) }

	first, _ := h.lookup(t)
	h.src.now = func() time.Time { return time.Unix(0, 0).Add(1000 * time.Hour) }
	second, ok := h.lookup(t)

	if !ok || first != second {
		t.Fatalf("cached answer changed: %+v then %+v", first, second)
	}
	if h.fetcher.callCount() != 1 {
		t.Errorf("registry asked %d times for one immutable digest", h.fetcher.callCount())
	}
	if h.github.callCount() != 1 {
		t.Errorf("GitHub asked %d times for one immutable commit", h.github.callCount())
	}
}

func TestFailedLookupIsRetriedOnlyAfterTheBackoff(t *testing.T) {
	h := newHarness(t, githubLabels())
	h.fetcher.err = errors.New("registry unreachable")

	base := time.Unix(0, 0)
	h.src.now = func() time.Time { return base }
	h.lookup(t)
	h.lookup(t)
	if h.fetcher.callCount() != 1 {
		t.Fatalf("a fresh failure was re-fetched: %d calls", h.fetcher.callCount())
	}

	h.src.now = func() time.Time { return base.Add(failureRetry + time.Second) }
	h.fetcher.err = nil
	rev, ok := h.lookup(t)
	if !ok || rev.SHA != testSHA {
		t.Fatalf("Lookup after the backoff = %+v, %v; want the recovered answer", rev, ok)
	}
}

func TestCacheDropsItselfWhenOverfull(t *testing.T) {
	h := newHarness(t, githubLabels())
	h.src.mu.Lock()
	for i := range maxEntries {
		h.src.store(fmt.Sprintf("repo%d@sha256:x", i), Revision{}, false)
	}
	h.src.mu.Unlock()

	if _, ok := h.lookup(t); !ok {
		t.Fatal("Lookup failed against a full cache")
	}
	h.src.mu.Lock()
	size := len(h.src.entries)
	h.src.mu.Unlock()
	if size != 1 {
		t.Errorf("cache size after overflow = %d, want a fresh map with one entry", size)
	}
}

// ---- fills -----------------------------------------------------------------

// Several callers asking about the same digest at once must produce one
// registry fetch, not a stampede of anonymous pulls.
func TestConcurrentLookupsJoinOneFill(t *testing.T) {
	h := newHarness(t, githubLabels())
	gate := make(chan struct{})
	h.fetcher.gate = gate

	results := make(chan Revision, 2)
	for range 2 {
		go func() {
			rev, ok := h.lookup(t)
			if !ok {
				t.Error("joined lookup reported unknown")
			}
			results <- rev
		}()
	}

	// Both callers must be parked on the same fill before it is released.
	deadline := time.Now().Add(2 * time.Second)
	for h.fetcher.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no fill ever started")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	close(gate)

	for range 2 {
		if rev := <-results; rev.SHA != testSHA {
			t.Errorf("joined lookup = %+v", rev)
		}
	}
	if got := h.fetcher.callCount(); got != 1 {
		t.Errorf("two concurrent lookups cost %d fetches, want one", got)
	}
}

// A caller's deadline bounds its wait, never the fill: the answer an impatient
// caller walked away from must still land in the cache for the next one.
func TestCallerTimeoutDoesNotAbortTheFill(t *testing.T) {
	h := newHarness(t, githubLabels())
	gate := make(chan struct{})
	h.fetcher.gate = gate

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, ok := h.src.Lookup(cancelled, "registry.example.com/acme/web", testDigest); ok {
		t.Fatal("a cancelled lookup claimed an answer")
	}

	close(gate)
	rev, ok := h.lookup(t)
	if !ok || rev.SHA != testSHA {
		t.Fatalf("Lookup after the abandoned fill = %+v, %v; want the cached success", rev, ok)
	}
	if got := h.fetcher.callCount(); got != 1 {
		t.Errorf("the abandoned fill was thrown away and re-fetched: %d fetches", got)
	}
}

func TestConcurrentLookupsAreSafe(t *testing.T) {
	h := newHarness(t, githubLabels())

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 32 {
				h.src.Lookup(t.Context(),
					fmt.Sprintf("registry.example.com/acme/web%d", i%3), testDigest)
				h.lookup(t)
			}
		}()
	}
	wg.Wait()
}
