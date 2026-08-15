package deploy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/journal"
	"github.com/SnowballSH/snowdeploy/internal/manifest"
	"github.com/SnowballSH/snowdeploy/internal/reconcile"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	testRepoWebURL = "https://github.com/acme/config"
)

func manifestYAML(digest string) string {
	return fmt.Sprintf(`name: web
image:
  repository: registry.example.com/acme/web
  digest: %s
template: web.container.tmpl
port: 8080
network: acme
health:
  url: http://127.0.0.1:8080/healthz
`, digest)
}

// ---- fakes -----------------------------------------------------------------

type fakeRepo struct {
	mu        sync.Mutex
	raw       map[string]string
	templates map[string]string
	syncs     int
	syncErr   error
}

func newFakeRepo(digest string) *fakeRepo {
	return &fakeRepo{
		raw:       map[string]string{"web": manifestYAML(digest)},
		templates: map[string]string{"web.container.tmpl": "[Container]\nImage={{.Image.Repository}}@{{.Image.Digest}}\n"},
	}
}

func (r *fakeRepo) Sync(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncs++
	if r.syncErr != nil {
		return "", r.syncErr
	}
	return "headsha", nil
}

func (r *fakeRepo) Manifest(service string) (*manifest.Manifest, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.raw[service]
	if !ok {
		return nil, nil, fmt.Errorf("no manifest for %s", service)
	}
	m, err := manifest.Parse([]byte(raw))
	if err != nil {
		return nil, nil, err
	}
	return m, []byte(raw), nil
}

func (r *fakeRepo) Template(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.templates[name]
	if !ok {
		return "", fmt.Errorf("no template %s", name)
	}
	return t, nil
}

func (r *fakeRepo) Services() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for name := range r.raw {
		out = append(out, name)
	}
	return out, nil
}

// merge simulates a PR landing on main.
func (r *fakeRepo) merge(service string, content []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.raw[service] = string(content)
}

type prCall struct {
	Service string
	Content string
	Title   string
}

type fakePR struct {
	mu sync.Mutex

	opened   []prCall
	merged   []int
	closed   []int
	comments []string
	next     int

	checksOK     bool
	checksDetail string
	checksErr    error
	mergeErr     error

	// mergeErrTimes bounds how many merges fail before succeeding; 0 with a
	// non-nil mergeErr means every merge fails. updated counts UpdateBranch
	// calls; updateErr makes them fail.
	mergeErrTimes int
	mergeFailed   int
	updated       []int
	updateErr     error

	// onOpen lets a test see the merged content land on the fake repo;
	// onMerge lets one observe the span between them.
	onOpen  func(service string, content []byte)
	onMerge func()
}

func (p *fakePR) OpenManifestPR(
	_ context.Context, service string, content []byte, title, _ string,
) (int, error) {
	p.mu.Lock()
	p.next++
	n := p.next
	p.opened = append(p.opened, prCall{Service: service, Content: string(content), Title: title})
	onOpen := p.onOpen
	p.mu.Unlock()

	if onOpen != nil {
		onOpen(service, content)
	}
	return n, nil
}

func (p *fakePR) WaitChecks(_ context.Context, _ int, _ time.Duration) (bool, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checksOK, p.checksDetail, p.checksErr
}

func (p *fakePR) Merge(_ context.Context, n int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mergeErr != nil {
		if p.mergeErrTimes == 0 || p.mergeFailed < p.mergeErrTimes {
			p.mergeFailed++
			return "", p.mergeErr
		}
	}
	p.merged = append(p.merged, n)
	onMerge := p.onMerge
	if onMerge != nil {
		onMerge()
	}
	return fmt.Sprintf("mergesha%d", n), nil
}

func (p *fakePR) UpdateBranch(_ context.Context, n int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.updateErr != nil {
		return p.updateErr
	}
	p.updated = append(p.updated, n)
	return nil
}

func (p *fakePR) ClosePR(_ context.Context, n int, comment string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = append(p.closed, n)
	p.comments = append(p.comments, comment)
	return nil
}

func (p *fakePR) snapshot() ([]prCall, []int, []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]prCall(nil), p.opened...),
		append([]int(nil), p.merged...),
		append([]int(nil), p.closed...)
}

type fakeApplier struct {
	mu      sync.Mutex
	applied []string
	failOn  map[string]error
	gate    chan struct{}
}

func (a *fakeApplier) Apply(
	_ context.Context, m *manifest.Manifest, _ string, onPhase reconcile.PhaseFunc,
) error {
	a.mu.Lock()
	a.applied = append(a.applied, m.Image.Digest)
	gate := a.gate
	err := a.failOn[m.Image.Digest]
	a.mu.Unlock()

	if gate != nil {
		<-gate
	}
	if onPhase != nil {
		onPhase(reconcile.PhaseProbing)
	}
	return err
}

func (a *fakeApplier) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.applied...)
}

type fakeInspector struct {
	mu      sync.Mutex
	running map[string]string
}

func (i *fakeInspector) RunningImage(_ context.Context, service string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	d, ok := i.running[service]
	if !ok {
		return "", errors.New("not running")
	}
	return d, nil
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	engine  *Engine
	repo    *fakeRepo
	pr      *fakePR
	applier *fakeApplier
	insp    *fakeInspector
	jrnl    *journal.Journal

	mu     sync.Mutex
	events []Event
	done   chan struct{}
	closed bool
}

func newHarness(t *testing.T, startDigest string) *harness {
	t.Helper()

	j, err := journal.Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	h := &harness{
		repo:    newFakeRepo(startDigest),
		pr:      &fakePR{checksOK: true},
		applier: &fakeApplier{failOn: map[string]error{}},
		insp:    &fakeInspector{running: map[string]string{"web": startDigest}},
		jrnl:    j,
		done:    make(chan struct{}),
	}
	h.pr.onOpen = func(service string, content []byte) { h.repo.merge(service, content) }

	h.engine = New(t.Context(), Options{
		Repo:           h.repo,
		PR:             h.pr,
		Applier:        h.applier,
		Journal:        j,
		Inspector:      h.insp,
		Notify:         h.record,
		CheckPoll:      time.Millisecond,
		RevertAttempts: 3,
		RevertBackoff:  time.Millisecond,
		RepoWebURL:     testRepoWebURL,
	})
	t.Cleanup(h.engine.Wait)
	return h
}

func (h *harness) record(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, e)
	if !h.closed && isTerminal(e.State) {
		h.closed = true
		close(h.done)
	}
}

// reset clears the recorded events so the next flow can be observed on its own.
func (h *harness) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.done = make(chan struct{})
	h.closed = false
	h.events = nil
}

func (h *harness) eventList() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Event(nil), h.events...)
}

func (h *harness) states() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.events {
		out = append(out, e.State)
	}
	return out
}

func (h *harness) waitTerminal(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	done := h.done
	h.mu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("no terminal state within 5s; states so far: %v", h.states())
	}
}

func (h *harness) entry(t *testing.T, id int64) journal.Entry {
	t.Helper()
	entries, err := h.jrnl.Recent("web", 50)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("journal entry %d not found", id)
	return journal.Entry{}
}

// ---- tests -----------------------------------------------------------------

func TestDeployHappyPathStateSequence(t *testing.T) {
	h := newHarness(t, digestA)

	id, err := h.engine.Deploy(t.Context(), "web", digestB, "admin")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	h.waitTerminal(t)

	want := []string{
		StatePROpen, StateChecks, StateMerged,
		StateReconciling, StateProbing, StateHealthy,
	}
	got := h.states()
	if len(got) != len(want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	e := h.entry(t, id)
	if e.State != journal.StateHealthy {
		t.Errorf("journal state = %q, want healthy", e.State)
	}
	if e.OldDigest != digestA || e.NewDigest != digestB {
		t.Errorf("journal digests = %s -> %s", e.OldDigest, e.NewDigest)
	}
	if e.Actor != "admin" {
		t.Errorf("actor = %q", e.Actor)
	}
	if e.Action != journal.ActionDeploy {
		t.Errorf("action = %q", e.Action)
	}
	if e.PRNumber == 0 || e.MergeSHA == "" {
		t.Errorf("journal lost the PR trail: pr=%d merge=%q", e.PRNumber, e.MergeSHA)
	}
	if e.FinishedAt.IsZero() {
		t.Error("journal entry never finished")
	}

	if applied := h.applier.calls(); len(applied) != 1 || applied[0] != digestB {
		t.Errorf("applied = %v, want [%s]", applied, digestB)
	}

	opened, merged, closed := h.pr.snapshot()
	if len(opened) != 1 || !strings.Contains(opened[0].Content, digestB) {
		t.Errorf("PR content did not carry the new digest: %+v", opened)
	}
	if strings.Contains(opened[0].Content, digestA) {
		t.Errorf("PR content still carries the old digest: %q", opened[0].Content)
	}
	if len(merged) != 1 {
		t.Errorf("merged = %v, want one merge", merged)
	}
	if len(closed) != 0 {
		t.Errorf("a successful deploy closed a PR: %v", closed)
	}
}

// A client that joins the stream mid-deploy still needs the links, so once
// the run learns its pull request and merge commit, every later event carries
// them — not just the transition that announced them.
func TestEventsCarryThePullRequestTrail(t *testing.T) {
	h := newHarness(t, digestA)

	if _, err := h.engine.Deploy(t.Context(), "web", digestB, "admin"); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	h.waitTerminal(t)

	wantPRURL := testRepoWebURL + "/pull/1"
	wantMergeURL := testRepoWebURL + "/commit/mergesha1"
	for _, ev := range h.eventList() {
		if ev.PRNumber != 1 || ev.PRURL != wantPRURL {
			t.Errorf("%s event pr trail = #%d %q, want #1 %q",
				ev.State, ev.PRNumber, ev.PRURL, wantPRURL)
		}
		merged := ev.State != StatePROpen && ev.State != StateChecks
		if merged && ev.MergeURL != wantMergeURL {
			t.Errorf("%s event merge url = %q, want %q", ev.State, ev.MergeURL, wantMergeURL)
		}
		if !merged && ev.MergeURL != "" {
			t.Errorf("%s event carries a merge url before the merge: %q", ev.State, ev.MergeURL)
		}
	}
}

func TestFailedChecksNeverTouchTheHost(t *testing.T) {
	h := newHarness(t, digestA)
	h.pr.checksOK = false
	h.pr.checksDetail = `check "verify" concluded failure`

	id, err := h.engine.Deploy(t.Context(), "web", digestB, "admin")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	h.waitTerminal(t)

	if got := h.states(); got[len(got)-1] != StateFailed {
		t.Fatalf("final state = %v, want failed", got)
	}
	if applied := h.applier.calls(); len(applied) != 0 {
		t.Fatalf("host was touched on a red check: %v", applied)
	}

	_, merged, closed := h.pr.snapshot()
	if len(merged) != 0 {
		t.Errorf("a red PR was merged: %v", merged)
	}
	if len(closed) != 1 {
		t.Errorf("the red PR was not closed: %v", closed)
	}

	e := h.entry(t, id)
	if e.State != journal.StateFailed {
		t.Errorf("journal state = %q, want failed", e.State)
	}
	if !strings.Contains(e.Detail, "verify") {
		t.Errorf("journal detail lost the reason: %q", e.Detail)
	}
}

func TestProbeFailureRollsBackAndRevertsMain(t *testing.T) {
	h := newHarness(t, digestA)
	h.applier.failOn[digestB] = errors.New("web did not become healthy")

	id, err := h.engine.Deploy(t.Context(), "web", digestB, "admin")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	h.waitTerminal(t)

	if got := h.states(); got[len(got)-1] != StateRolledBack {
		t.Fatalf("final state = %v, want rolled-back", got)
	}

	applied := h.applier.calls()
	if len(applied) != 2 || applied[0] != digestB || applied[1] != digestA {
		t.Fatalf("applied = %v, want [new old] to restore the previous digest", applied)
	}

	opened, merged, _ := h.pr.snapshot()
	if len(opened) != 2 {
		t.Fatalf("opened %d PRs, want the deploy plus the revert", len(opened))
	}
	if !strings.Contains(opened[1].Title, "revert") {
		t.Errorf("second PR is not a revert: %q", opened[1].Title)
	}
	if !strings.Contains(opened[1].Content, digestA) {
		t.Errorf("revert PR does not restore the old digest: %q", opened[1].Content)
	}
	if len(merged) != 2 {
		t.Errorf("revert PR was not merged: %v", merged)
	}

	e := h.entry(t, id)
	if e.State != journal.StateRolledBack {
		t.Errorf("journal state = %q, want rolled-back", e.State)
	}
	if !strings.Contains(e.Detail, "healthy") {
		t.Errorf("journal detail lost the probe failure: %q", e.Detail)
	}
}

func TestRollbackAlsoFailingLeavesFailedState(t *testing.T) {
	h := newHarness(t, digestA)
	h.applier.failOn[digestB] = errors.New("probe red")
	h.applier.failOn[digestA] = errors.New("old image gone too")

	id, err := h.engine.Deploy(t.Context(), "web", digestB, "admin")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	h.waitTerminal(t)

	if got := h.states(); got[len(got)-1] != StateFailed {
		t.Fatalf("final state = %v, want failed", got)
	}
	e := h.entry(t, id)
	if !strings.Contains(e.Detail, "old image gone too") {
		t.Errorf("journal detail lost the rollback failure: %q", e.Detail)
	}
}

func TestSecondDeployQueuesBehindTheFirst(t *testing.T) {
	h := newHarness(t, digestA)
	gate := make(chan struct{})
	h.applier.gate = gate

	if _, err := h.engine.Deploy(t.Context(), "web", digestB, "admin"); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	// Wait until the first flow is inside Apply.
	deadline := time.Now().Add(2 * time.Second)
	for len(h.applier.calls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first deploy never reached Apply")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := h.engine.Deploy(t.Context(), "web", digestC, "admin"); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	opened, _, _ := h.pr.snapshot()
	if len(opened) != 1 {
		t.Fatalf("second deploy opened a PR while the first was in flight: %d PRs", len(opened))
	}

	close(gate)
	h.applier.mu.Lock()
	h.applier.gate = nil
	h.applier.mu.Unlock()

	deadline = time.Now().Add(5 * time.Second)
	for {
		opened, _, _ = h.pr.snapshot()
		if len(opened) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second deploy never ran: %d PRs", len(opened))
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(opened[1].Content, digestC) {
		t.Errorf("queued deploy carried the wrong digest: %q", opened[1].Content)
	}
}

func TestRollbackWithNoTargetUsesLastHealthyDigest(t *testing.T) {
	h := newHarness(t, digestA)

	if _, err := h.engine.Deploy(t.Context(), "web", digestB, "admin"); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	h.waitTerminal(t)

	h.reset()

	// main now pins digestB. A second deploy makes digestC current and leaves
	// digestB as the newest healthy digest that is *not* running.
	if _, err := h.engine.Deploy(t.Context(), "web", digestC, "operator"); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	h.waitTerminal(t)

	h.reset()

	id, err := h.engine.Rollback(t.Context(), "web", "", "operator")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	h.waitTerminal(t)

	e := h.entry(t, id)
	if e.Action != journal.ActionRollback {
		t.Errorf("action = %q, want rollback", e.Action)
	}
	if e.NewDigest != digestB {
		t.Errorf("rollback target = %s, want the previous healthy digest %s", e.NewDigest, digestB)
	}
	if e.State != journal.StateHealthy {
		t.Errorf("rollback state = %q", e.State)
	}
}

func TestRollbackWithNoHistoryFailsClosed(t *testing.T) {
	h := newHarness(t, digestA)
	if _, err := h.engine.Rollback(t.Context(), "web", "", "operator"); err == nil {
		t.Fatal("rollback succeeded with no healthy history")
	}
	if applied := h.applier.calls(); len(applied) != 0 {
		t.Fatalf("host was touched: %v", applied)
	}
}

func TestDeployRejectsTheDigestAlreadyPinned(t *testing.T) {
	h := newHarness(t, digestA)
	_, err := h.engine.Deploy(t.Context(), "web", digestA, "admin")
	if !errors.Is(err, ErrAlreadyAtDigest) {
		t.Fatalf("err = %v, want ErrAlreadyAtDigest", err)
	}
}

func TestDeployRejectsInvalidDigest(t *testing.T) {
	h := newHarness(t, digestA)
	for _, d := range []string{"", "latest", "sha256:short", "registry/x:tag"} {
		if _, err := h.engine.Deploy(t.Context(), "web", d, "admin"); err == nil {
			t.Errorf("digest %q accepted", d)
		}
	}
}

func TestDeployRejectsUnknownService(t *testing.T) {
	h := newHarness(t, digestA)
	if _, err := h.engine.Deploy(t.Context(), "nope", digestB, "admin"); err == nil {
		t.Fatal("unknown service accepted")
	}
}

func TestDriftReportsMismatchAndSilenceOnMatch(t *testing.T) {
	h := newHarness(t, digestA)

	drift, err := h.engine.Drift(t.Context())
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if len(drift) != 0 {
		t.Fatalf("clean host reported drift: %v", drift)
	}

	h.insp.mu.Lock()
	h.insp.running["web"] = digestC
	h.insp.mu.Unlock()

	drift, err = h.engine.Drift(t.Context())
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if _, ok := drift["web"]; !ok {
		t.Fatalf("drift not reported: %v", drift)
	}
	if !strings.Contains(drift["web"], digestC) {
		t.Errorf("drift detail lost the running digest: %q", drift["web"])
	}
}

func TestDriftReportsAServiceThatIsNotRunning(t *testing.T) {
	h := newHarness(t, digestA)
	h.insp.mu.Lock()
	delete(h.insp.running, "web")
	h.insp.mu.Unlock()

	drift, err := h.engine.Drift(t.Context())
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if _, ok := drift["web"]; !ok {
		t.Fatalf("a stopped service is drift; got %v", drift)
	}
}

func TestSetDigestReplacesExactlyOneOccurrence(t *testing.T) {
	raw := []byte(manifestYAML(digestA))
	out, err := setDigest(raw, digestA, digestB)
	if err != nil {
		t.Fatalf("setDigest: %v", err)
	}
	if strings.Count(string(out), digestB) != 1 {
		t.Errorf("expected exactly one new digest:\n%s", out)
	}
	if strings.Contains(string(out), digestA) {
		t.Errorf("old digest survived:\n%s", out)
	}
	if _, err := setDigest(raw, digestC, digestB); err == nil {
		t.Error("setDigest accepted a digest that is not in the file")
	}
}

// The base moving underneath a proposal is a normal event under strict branch
// protection. One update-branch round must recover it end to end.
func TestRefusedMergeRecoversAfterBranchUpdate(t *testing.T) {
	h := newHarness(t, digestA)
	h.pr.mu.Lock()
	h.pr.mergeErr = errors.New(`405 Required status check "verify" is expected`)
	h.pr.mergeErrTimes = 1
	h.pr.mu.Unlock()

	id, err := h.engine.Deploy(t.Context(), "web", digestB, "admin")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	h.waitTerminal(t)

	e := h.entry(t, id)
	if e.State != journal.StateHealthy {
		t.Fatalf("journal state = %q (%s), want healthy after one retry", e.State, e.Detail)
	}
	_, _, closed := h.pr.snapshot()
	if len(closed) != 0 {
		t.Errorf("a recovered deploy closed its own pull request: %v", closed)
	}
	h.pr.mu.Lock()
	updated := len(h.pr.updated)
	h.pr.mu.Unlock()
	if updated != 1 {
		t.Errorf("UpdateBranch calls = %d, want 1", updated)
	}
}

// A merge that stays refused must end the deploy, close the proposal so it
// cannot strand as an open PR, and say how many rounds were tried.
func TestExhaustedMergeRetriesFailAndCloseThePR(t *testing.T) {
	h := newHarness(t, digestA)
	h.pr.mu.Lock()
	h.pr.mergeErr = errors.New("405 base branch was modified")
	h.pr.mu.Unlock()

	id, err := h.engine.Deploy(t.Context(), "web", digestB, "admin")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	h.waitTerminal(t)

	e := h.entry(t, id)
	if e.State != journal.StateFailed {
		t.Fatalf("journal state = %q, want failed", e.State)
	}
	if !strings.Contains(e.Detail, "after 3 attempts") {
		t.Errorf("detail does not say the retries were exhausted: %q", e.Detail)
	}
	_, _, closed := h.pr.snapshot()
	if len(closed) != 1 {
		t.Errorf("the stranded proposal was not closed: closed=%v", closed)
	}
}

// Two deploys of different services must not hold open proposals at the same
// time: an unserialized pair opens both from one base, and strict branch
// protection strands whichever merges second.
func TestConcurrentDeploysSerializeTheMergeSpan(t *testing.T) {
	h := newHarness(t, digestA)
	h.repo.mu.Lock()
	h.repo.raw["api"] = strings.Replace(manifestYAML(digestA), "name: web", "name: api", 1)
	h.repo.mu.Unlock()
	h.insp.mu.Lock()
	h.insp.running["api"] = digestA
	h.insp.mu.Unlock()

	var depth, worst atomic.Int64
	h.pr.mu.Lock()
	h.pr.onOpen = func(service string, content []byte) {
		if d := depth.Add(1); d > worst.Load() {
			worst.Store(d)
		}
		h.repo.merge(service, content)
	}
	h.pr.mu.Unlock()
	h.pr.onMerge = func() { depth.Add(-1) }

	if _, err := h.engine.Deploy(t.Context(), "web", digestB, "admin"); err != nil {
		t.Fatalf("Deploy web: %v", err)
	}
	if _, err := h.engine.Deploy(t.Context(), "api", digestB, "admin"); err != nil {
		t.Fatalf("Deploy api: %v", err)
	}
	h.engine.Wait()

	if worst.Load() != 1 {
		t.Fatalf("open-to-merge spans interleaved: max concurrent proposals = %d, want 1", worst.Load())
	}
	_, merged, _ := h.pr.snapshot()
	if len(merged) != 2 {
		t.Fatalf("merged = %v, want both proposals merged", merged)
	}
}
