package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/gitops"
	"github.com/SnowballSH/snowdeploy/internal/journal"
	"github.com/SnowballSH/snowdeploy/internal/manifest"
	"github.com/SnowballSH/snowdeploy/internal/reconcile"
)

// ErrAlreadyAtDigest is returned when a request would change nothing.
var ErrAlreadyAtDigest = errors.New("service is already pinned to that digest")

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ConfigRepo is the merged-state mirror the engine reads. SyncState exists
// because the error Sync returns can quote the remote URL — somewhere a
// credential can hide — while SyncState's reason is the repository's own safe
// vocabulary, fit for a journal detail every client reads.
type ConfigRepo interface {
	Sync(ctx context.Context) (string, error)
	SyncState() (synced bool, reason error)
	Manifest(service string) (*manifest.Manifest, []byte, error)
	Template(name string) (string, error)
	Services() ([]string, error)
}

// Applier walks a manifest onto the host.
type Applier interface {
	Apply(ctx context.Context, m *manifest.Manifest, tmpl string, onPhase reconcile.PhaseFunc) error
}

// Store is the journal surface the engine needs.
type Store interface {
	Begin(e journal.Entry) (int64, error)
	Progress(id int64, state, detail string) error
	Finish(id int64, state, detail string) error
	SetPR(id int64, prNumber int) error
	SetMergeSHA(id int64, sha string) error
	LastHealthyDigest(service string) (string, error)
	PreviousHealthyDigest(service, notDigest string) (string, error)
}

// Inspector reports what is actually running.
type Inspector interface {
	RunningImage(ctx context.Context, service string) (string, error)
}

// Event is one state transition, streamed live to the UI and CLI. The pull
// request and merge fields appear as soon as a run learns them and ride on
// every later event, so a client that joined mid-deploy still gets the links.
type Event struct {
	Service   string    `json:"service"`
	Action    string    `json:"action,omitempty"`
	State     string    `json:"state"`
	Detail    string    `json:"detail"`
	JournalID int64     `json:"journalId"`
	PRNumber  int       `json:"prNumber,omitempty"`
	PRURL     string    `json:"prUrl,omitempty"`
	MergeURL  string    `json:"mergeUrl,omitempty"`
	At        time.Time `json:"at"`
}

// Options are the engine's dependencies.
type Options struct {
	Repo           ConfigRepo
	PR             gitops.PRClient
	Applier        Applier
	Journal        Store
	Inspector      Inspector
	Notify         func(Event)
	CheckPoll      time.Duration
	RevertAttempts int
	RevertBackoff  time.Duration
	// MergeBackoff is the pause between a refused merge and the next
	// attempt, giving GitHub's asynchronous mergeability computation time
	// to settle.
	MergeBackoff time.Duration

	// RepoWebURL is the configuration repository's web address, for example
	// "https://github.com/acme/config". Events derive their pull-request and
	// merge-commit links from it; empty simply leaves the links off.
	RepoWebURL string
}

// Engine runs one deploy per service at a time.
type Engine struct {
	opts Options

	// baseCtx bounds every in-flight deploy. A deploy must outlive the HTTP
	// request that started it, so it deliberately does not use the caller's.
	baseCtx context.Context

	mu      sync.Mutex
	locks   map[string]chan struct{}
	pending map[string]pendingRun

	// mergeMu serializes the open-to-merge span across ALL services. The
	// configuration repository requires branches to be up to date with main,
	// so two proposals opened from the same base cannot both merge: whichever
	// lands first strands the other — witnessed 2026-08-15, when three
	// deploys clicked together produced one merge and two 405s. One proposal
	// in flight at a time is a merge queue of depth one, which is all four
	// services need.
	mergeMu sync.Mutex

	inFlight sync.WaitGroup
	queued   atomic.Int64
}

// New builds an engine whose background work is bounded by ctx.
func New(ctx context.Context, opts Options) *Engine {
	if opts.CheckPoll <= 0 {
		opts.CheckPoll = 15 * time.Second
	}
	if opts.RevertAttempts <= 0 {
		opts.RevertAttempts = 3
	}
	if opts.RevertBackoff <= 0 {
		opts.RevertBackoff = 30 * time.Second
	}
	if opts.MergeBackoff <= 0 {
		opts.MergeBackoff = 5 * time.Second
	}
	return &Engine{
		opts:    opts,
		baseCtx: ctx,
		locks:   make(map[string]chan struct{}),
		pending: make(map[string]pendingRun),
	}
}

// Wait blocks until every in-flight deploy has finished.
func (e *Engine) Wait() { e.inFlight.Wait() }

// QueueDepth is the number of requests waiting on a per-service lock.
func (e *Engine) QueueDepth() int64 { return e.queued.Load() }

// Deploy pins a service to a digest. It returns as soon as the journal entry
// exists; progress arrives as events. joined reports that the click matched a
// run already carrying the same digest and was answered with its id.
func (e *Engine) Deploy(
	_ context.Context, service, digest, actor string,
) (id int64, joined bool, err error) {
	if !digestPattern.MatchString(digest) {
		return 0, false, fmt.Errorf("%q is not a sha256 digest pin", digest)
	}
	return e.start(journal.ActionDeploy, service, actor,
		func(string) (string, error) { return digest, nil })
}

// Rollback re-pins a service to a previous digest. With no explicit target it
// uses the newest healthy digest that is not the one currently pinned — the
// digest the operator means by "roll back", not the one already running.
func (e *Engine) Rollback(
	_ context.Context, service, toDigest, actor string,
) (id int64, joined bool, err error) {
	if toDigest != "" && !digestPattern.MatchString(toDigest) {
		return 0, false, fmt.Errorf("%q is not a sha256 digest pin", toDigest)
	}
	return e.start(journal.ActionRollback, service, actor,
		func(current string) (string, error) {
			if toDigest != "" {
				return toDigest, nil
			}
			return e.opts.Journal.PreviousHealthyDigest(service, current)
		})
}

// Converge applies the manifest as main has it right now — same digest, fresh
// render — for the change a deploy cannot carry: an env, volume, or template
// edit that merged without moving the image pin. It opens no pull request; the
// change it applies already went through the configuration repository's review.
func (e *Engine) Converge(
	_ context.Context, service, actor string,
) (id int64, joined bool, err error) {
	return e.start(journal.ActionConverge, service, actor,
		func(current string) (string, error) { return current, nil })
}

// resolveTarget turns a request into the digest to pin, given what is pinned.
type resolveTarget func(currentDigest string) (string, error)

// start validates the request against the mirror as it stands and journals
// the run. It deliberately does not sync first: the fetch can take seconds or
// hang on an unreachable remote, and nothing would be visible anywhere until
// it finished. The run itself syncs as its first act, after the click already
// has a journal row and a detected event to show for itself.
func (e *Engine) start(
	action, service, actor string, resolve resolveTarget,
) (int64, bool, error) {
	current, raw, err := e.opts.Repo.Manifest(service)
	if err != nil {
		return 0, false, err
	}

	digest, err := resolve(current.Image.Digest)
	if err != nil {
		return 0, false, err
	}
	if !digestPattern.MatchString(digest) {
		return 0, false, fmt.Errorf("%q is not a sha256 digest pin", digest)
	}
	// For converge the pinned digest is the point, and there is no content
	// change to propose: the run re-renders and re-applies what main already
	// holds.
	newContent := raw
	if action != journal.ActionConverge {
		if current.Image.Digest == digest {
			return 0, false, fmt.Errorf("%s: %w (%s)", service, ErrAlreadyAtDigest, digest)
		}
		newContent, err = setDigest(raw, current.Image.Digest, digest)
		if err != nil {
			return 0, false, err
		}
	}

	// A second click for a target already queued or running is the same
	// intent, not a second deploy: answer with the run that is already
	// carrying it. The check, the journal write, and the registration are one
	// critical section — with the lock dropped around Begin, two identical
	// clicks both pass the check and both journal a run, and the second
	// registration would orphan the first's entry mid-flight. Begin is one
	// fast local SQLite write, cheap enough to hold the lock across.
	e.mu.Lock()
	pending, live := e.pending[service]
	// A converge only ever joins a converge: a pending deploy at the same
	// digest froze its manifest before the edit the converge was clicked for,
	// so answering with that run could leave the edit unapplied. Deploys and
	// rollbacks keep joining each other — both mean "pin this digest".
	sameKind := (pending.action == journal.ActionConverge) ==
		(action == journal.ActionConverge)
	if live && sameKind && pending.digest == digest {
		e.mu.Unlock()
		return pending.id, true, nil
	}
	id, err := e.opts.Journal.Begin(journal.Entry{
		Service:   service,
		Action:    action,
		Actor:     actor,
		OldDigest: current.Image.Digest,
		NewDigest: digest,
		State:     StateDetected,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		e.mu.Unlock()
		return 0, false, err
	}
	// A live entry for a different digest is not ours to overwrite: it is the
	// dedup point for every click still matching it. This run proceeds to the
	// queue unregistered and simply offers no join point of its own.
	if !live {
		e.pending[service] = pendingRun{action: action, digest: digest, id: id}
	}
	e.mu.Unlock()

	e.inFlight.Add(1)
	go func() {
		defer e.inFlight.Done()
		defer func() {
			e.mu.Lock()
			if pending, ok := e.pending[service]; ok && pending.id == id {
				delete(e.pending, service)
			}
			e.mu.Unlock()
		}()
		e.run(runRequest{
			id:          id,
			action:      action,
			service:     service,
			oldDigest:   current.Image.Digest,
			newDigest:   digest,
			originalRaw: raw,
			newContent:  newContent,
		})
	}()
	return id, false, nil
}

// pendingRun is a run that has a journal entry but has not yet finished.
type pendingRun struct {
	action string
	digest string
	id     int64
}

type runRequest struct {
	id          int64
	action      string
	service     string
	oldDigest   string
	newDigest   string
	originalRaw []byte
	newContent  []byte

	// The pull-request trail, filled in as run learns it. Living on the
	// request means every later emit carries it without re-reading the journal.
	prNumber int
	prURL    string
	mergeURL string

	// detailPrefix rides in front of every emitted detail. The revert flow
	// reuses mergeThroughChecks, whose sentences describe advancing a deploy;
	// the prefix keeps them honest when the same steps are undoing one.
	detailPrefix string
}

func (e *Engine) run(req runRequest) {
	ctx := e.baseCtx

	// The click must become visible before the fetch and before any queue is
	// waited on: a deploy parked behind a slow remote or another service's
	// merge span used to render nothing at all, which read as a dead button
	// and invited duplicate clicks.
	e.emit(req, StateDetected, "reading the configuration repository")
	if _, err := e.opts.Repo.Sync(ctx); err != nil {
		e.fail(req, e.syncFailure(err))
		return
	}

	queuedDetail := "queued for the merge queue"
	if req.action == journal.ActionConverge {
		queuedDetail = "queued behind the service's in-flight run"
	}
	e.emit(req, StateDetected, queuedDetail)
	release, err := e.lock(ctx, req.service)
	if err != nil {
		detail := fmt.Sprintf("queued %s abandoned: %v", req.action, err)
		if errors.Is(err, context.Canceled) {
			detail += "; the daemon was restarting; re-run"
		}
		e.fail(req, detail)
		return
	}
	defer release()

	// A converge proposes nothing, so it has no business in the merge queue:
	// it re-reads main at the front of its own service's queue and applies it.
	if req.action == journal.ActionConverge {
		e.converge(ctx, req)
		return
	}

	e.lockMergeQueue()

	// The manifest this run froze at click time is stale by the time it
	// reaches the front of the queue: other deploys landed while it waited.
	// Everything from here on must be computed against the file as it is now,
	// or the proposal silently reverts whatever landed in between — and a
	// rollback would restore the click-time digest, undoing an interleaved
	// healthy deploy.
	current, raw, err := e.opts.Repo.Manifest(req.service)
	if err != nil {
		e.mergeMu.Unlock()
		e.fail(req, fmt.Sprintf(
			"could not re-read the manifest at the front of the queue: %v", err))
		return
	}

	if current.Image.Digest == req.newDigest {
		// An earlier deploy may have already landed the same digest, and
		// proposing the change again would be an empty pull request that
		// fails on its own emptiness. But manifest equality alone is not
		// health: during a revert window main still pins the digest that just
		// failed its probe, and finishing healthy here would poison
		// LastHealthyDigest with it. Only what is actually running settles it.
		running, runErr := e.opts.Inspector.RunningImage(ctx, req.service)
		e.mergeMu.Unlock()
		if runErr == nil && running == req.newDigest {
			e.finish(req, StateHealthy, fmt.Sprintf(
				"%s was already brought to %s by an earlier deploy; nothing to do",
				req.service, req.newDigest))
			return
		}
		e.emit(req, StateReconciling, fmt.Sprintf(
			"main already pins %s but the host does not run it; applying the merged manifest",
			req.newDigest))
		if applyErr := e.applyMerged(ctx, req); applyErr != nil {
			e.rollback(ctx, req, applyErr)
			return
		}
		e.finish(req, StateHealthy, fmt.Sprintf(
			"%s is healthy on %s", req.service, req.newDigest))
		return
	}

	fresh, err := setDigest(raw, current.Image.Digest, req.newDigest)
	if err != nil {
		e.mergeMu.Unlock()
		e.fail(req,
			"the manifest changed while this deploy was queued; "+
				"re-run to propose it against the current file")
		return
	}
	req.newContent = fresh
	req.originalRaw = raw
	req.oldDigest = current.Image.Digest

	prNumber, err := e.opts.PR.OpenManifestPR(ctx, req.service, req.newContent,
		prTitle(req.action, req.service, req.newDigest),
		prBody(req.action, req.service, req.oldDigest, req.newDigest))
	if err != nil {
		e.mergeMu.Unlock()
		e.fail(req, fmt.Sprintf("could not open the manifest pull request: %v", err))
		return
	}
	if err := e.opts.Journal.SetPR(req.id, prNumber); err != nil {
		e.mergeMu.Unlock()
		e.fail(req, fmt.Sprintf("could not record the pull request: %v", err))
		return
	}
	req.prNumber = prNumber
	if e.opts.RepoWebURL != "" {
		req.prURL = e.opts.RepoWebURL + "/pull/" + strconv.Itoa(prNumber)
	}
	e.emit(req, StatePROpen, fmt.Sprintf("pull request #%d opened", prNumber))

	mergeSHA, err := e.mergeThroughChecks(ctx, req, prNumber)
	e.mergeMu.Unlock()
	if err != nil {
		e.closePR(ctx, prNumber, fmt.Sprintf("snowdeploy abandoned this change: %v", err))
		e.fail(req, err.Error())
		return
	}
	if err := e.opts.Journal.SetMergeSHA(req.id, mergeSHA); err != nil {
		e.fail(req, fmt.Sprintf("could not record the merge: %v", err))
		return
	}
	if e.opts.RepoWebURL != "" {
		req.mergeURL = e.opts.RepoWebURL + "/commit/" + mergeSHA
	}
	e.emit(req, StateMerged, "merged as "+mergeSHA)

	e.emit(req, StateReconciling, "applying the merged manifest")
	applyErr := e.applyMerged(ctx, req)
	if applyErr == nil {
		e.finish(req, StateHealthy, fmt.Sprintf(
			"%s is healthy on %s", req.service, req.newDigest))
		return
	}

	e.rollback(ctx, req, applyErr)
}

// mergeAttempts bounds how often a refused merge is retried after updating
// the branch. Each retry re-waits the full check run, so two retries already
// spans several minutes of base-branch churn — more would mean the repository
// is moving too fast for a deploy to land at all, which a human should see.
const mergeAttempts = 3

// mergeThroughChecks waits for the proposal's checks and merges it, updating
// the branch and re-running the checks when the base has moved underneath it.
// The caller holds mergeMu, so only external pushes can move the base here.
func (e *Engine) mergeThroughChecks(
	ctx context.Context, req runRequest, prNumber int,
) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= mergeAttempts; attempt++ {
		if attempt == 1 {
			e.emit(req, StateChecks, "waiting for the configuration repository's checks")
		} else {
			e.emit(req, StateChecks, fmt.Sprintf(
				"main moved underneath the proposal; branch updated, re-running checks (attempt %d of %d)",
				attempt, mergeAttempts))
		}
		ok, detail, err := e.opts.PR.WaitChecks(ctx, prNumber, e.opts.CheckPoll)
		if err != nil {
			return "", fmt.Errorf("checks could not be read: %w", err)
		}
		if !ok {
			return "", fmt.Errorf("%s; see pull request #%d", detail, prNumber)
		}
		mergeSHA, err := e.opts.PR.Merge(ctx, prNumber)
		if err == nil {
			return mergeSHA, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", fmt.Errorf("merge failed: %w", err)
		}
		if updateErr := e.opts.PR.UpdateBranch(ctx, prNumber); updateErr != nil {
			return "", fmt.Errorf("merge failed (%w) and the branch could not be updated: %w",
				err, updateErr)
		}
		// GitHub computes mergeability asynchronously after checks conclude
		// and after a branch updates, and answers "not mergeable" while it
		// does — merging again immediately just re-asks the stale question.
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("merge failed: %w", lastErr)
		case <-time.After(e.opts.MergeBackoff):
		}
	}
	return "", fmt.Errorf("merge failed after %d attempts: %w", mergeAttempts, lastErr)
}

// syncFailure words a failed sync for the journal and the stream. The error
// itself never travels: a Git transport error quotes the remote URL, and a
// URL is somewhere a credential can hide. What travels is the repository's
// own reason, plus the remedy when the failure names one.
func (e *Engine) syncFailure(err error) string {
	detail := "the configuration repository could not be read"
	if _, reason := e.opts.Repo.SyncState(); reason != nil {
		detail = reason.Error()
	}
	if errors.Is(err, gitops.ErrCredentialUnavailable) {
		detail += "; re-run once the secret store is unsealed"
	}
	return detail
}

// converge applies the manifest as main holds it at the front of the queue.
// Interleaved deploys may have moved the pin while this run waited; applying
// the file as it is now is the contract, so the emitted details name the
// digest actually applied even when the receipt's Begin-time digests lag it.
//
// There is no rollback from here: the unit the host ran before this converge
// is recorded nowhere, so a failed probe finishes failed and says so.
func (e *Engine) converge(ctx context.Context, req runRequest) {
	m, _, err := e.opts.Repo.Manifest(req.service)
	if err != nil {
		e.fail(req, fmt.Sprintf(
			"could not re-read the manifest at the front of the queue: %v", err))
		return
	}
	req.newDigest = m.Image.Digest
	e.emit(req, StateReconciling, fmt.Sprintf(
		"re-rendering the merged manifest, digest unchanged at %s",
		shortDigest(m.Image.Digest)))
	if err := e.applyManifest(ctx, req, m); err != nil {
		e.fail(req, fmt.Sprintf(
			"%v; a converge has no previous unit to restore — fix the manifest "+
				"and converge again, or deploy a known-good digest", err))
		return
	}
	e.finish(req, StateHealthy, fmt.Sprintf(
		"%s converged; healthy on %s", req.service, req.newDigest))
}

// applyMerged pulls the merged branch and walks it onto the host.
func (e *Engine) applyMerged(ctx context.Context, req runRequest) error {
	if _, err := e.opts.Repo.Sync(ctx); err != nil {
		return fmt.Errorf("sync merged state: %s", e.syncFailure(err))
	}
	m, _, err := e.opts.Repo.Manifest(req.service)
	if err != nil {
		return err
	}
	if m.Image.Digest != req.newDigest {
		return fmt.Errorf(
			"merged manifest pins %s, not the requested %s", m.Image.Digest, req.newDigest)
	}
	return e.applyManifest(ctx, req, m)
}

// applyManifest walks one manifest onto the host, streaming the phases.
func (e *Engine) applyManifest(
	ctx context.Context, req runRequest, m *manifest.Manifest,
) error {
	tmpl, err := e.opts.Repo.Template(m.Template)
	if err != nil {
		return err
	}
	return e.opts.Applier.Apply(ctx, m, tmpl, func(phase string) {
		switch phase {
		case reconcile.PhaseRestarting:
			e.emit(req, StateReconciling, fmt.Sprintf(
				"restarting %s (pulling image)", req.service))
		case reconcile.PhaseProbing:
			e.emit(req, StateProbing, "probing "+m.Health.URL)
		}
	})
}

// rollback restores the previously running digest on the host, then brings
// main back in line with it so the record keeps describing what runs.
//
// Every event from here on rides on a copy whose pull-request trail is
// cleared: the proposal those fields point at is the change being undone, and
// carrying it further would render the revert's own progress as the failed
// deploy walking backwards through "checks" with a link nobody can act on.
func (e *Engine) rollback(ctx context.Context, req runRequest, cause error) {
	undo := req
	undo.prNumber, undo.prURL, undo.mergeURL = 0, "", ""

	e.emit(undo, StateReconciling, fmt.Sprintf(
		"probe failed; restoring %s on the host", shortDigest(req.oldDigest)))
	if restoreErr := e.restorePrevious(ctx, undo); restoreErr != nil {
		e.finish(undo, StateFailed, fmt.Sprintf(
			"%v; the automatic rollback also failed: %v", cause, restoreErr))
		return
	}

	detail := fmt.Sprintf("%v; rolled back to %s", cause, req.oldDigest)
	if err := e.revertMain(ctx, &undo); err != nil {
		detail += fmt.Sprintf("; main still pins %s and needs a manual revert (%v)",
			req.newDigest, err)
	} else {
		detail += "; main reverted"
	}
	undo.detailPrefix = ""
	e.finish(undo, StateRolledBack, detail)
}

func (e *Engine) restorePrevious(ctx context.Context, req runRequest) error {
	m, err := manifest.Parse(req.originalRaw)
	if err != nil {
		return fmt.Errorf("parse the previous manifest: %w", err)
	}
	tmpl, err := e.opts.Repo.Template(m.Template)
	if err != nil {
		return err
	}
	return e.opts.Applier.Apply(ctx, m, tmpl, func(phase string) {
		if phase == reconcile.PhaseProbing {
			e.emit(req, StateProbing, "probing "+m.Health.URL)
		}
	})
}

// revertMain opens and merges a pull request restoring the previous manifest,
// retrying a bounded number of times before giving up loudly. It mutates req's
// pull-request trail: each attempt clears it, and the attempt that opens a
// revert proposal fills it in, so events during the revert link the pull
// request that is actually in flight.
func (e *Engine) revertMain(ctx context.Context, req *runRequest) error {
	req.detailPrefix = "reverting main: "
	var last error
	for attempt := 1; attempt <= e.opts.RevertAttempts; attempt++ {
		req.prNumber, req.prURL, req.mergeURL = 0, "", ""
		e.emit(*req, StateReconciling, fmt.Sprintf(
			"attempt %d of %d", attempt, e.opts.RevertAttempts))
		last = e.revertOnce(ctx, req)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(e.opts.RevertBackoff * time.Duration(attempt)):
		}
	}
	return fmt.Errorf("after %d attempts: %w", e.opts.RevertAttempts, last)
}

func (e *Engine) revertOnce(ctx context.Context, req *runRequest) error {
	e.lockMergeQueue()
	defer e.mergeMu.Unlock()
	prNumber, err := e.opts.PR.OpenManifestPR(ctx, req.service, req.originalRaw,
		fmt.Sprintf("revert %s to %s", req.service, shortDigest(req.oldDigest)),
		fmt.Sprintf(
			"Automatic revert: %s failed its health probe on %s and was rolled back "+
				"to %s on the host. This restores the manifest to what is running.",
			req.service, req.newDigest, req.oldDigest))
	if err != nil {
		return err
	}
	req.prNumber = prNumber
	if e.opts.RepoWebURL != "" {
		req.prURL = e.opts.RepoWebURL + "/pull/" + strconv.Itoa(prNumber)
	}
	if _, err := e.mergeThroughChecks(ctx, *req, prNumber); err != nil {
		return err
	}
	return nil
}

// Drift compares every merged manifest against what is actually running.
func (e *Engine) Drift(ctx context.Context) (map[string]string, error) {
	if _, err := e.opts.Repo.Sync(ctx); err != nil {
		return nil, fmt.Errorf("sync configuration repository: %w", err)
	}
	services, err := e.opts.Repo.Services()
	if err != nil {
		return nil, err
	}

	drift := make(map[string]string)
	for _, service := range services {
		m, _, err := e.opts.Repo.Manifest(service)
		if err != nil {
			drift[service] = fmt.Sprintf("manifest unreadable: %v", err)
			continue
		}
		running, err := e.opts.Inspector.RunningImage(ctx, service)
		if err != nil {
			drift[service] = fmt.Sprintf("not running: %v", err)
			continue
		}
		if running != m.Image.Digest {
			drift[service] = fmt.Sprintf(
				"running %s, manifest pins %s", running, m.Image.Digest)
		}
	}
	return drift, nil
}

// ---- plumbing --------------------------------------------------------------

// lockMergeQueue takes the cross-service merge lock, counting the wait in
// the same gauge as the per-service queue: a deploy parked behind another
// service's merge span is queued in every sense the gauge's reader cares
// about, and it used to wait there invisibly.
func (e *Engine) lockMergeQueue() {
	e.queued.Add(1)
	defer e.queued.Add(-1)
	e.mergeMu.Lock()
}

// lock serializes deploys per service. Later requests queue rather than race.
func (e *Engine) lock(ctx context.Context, service string) (func(), error) {
	e.mu.Lock()
	ch, ok := e.locks[service]
	if !ok {
		ch = make(chan struct{}, 1)
		e.locks[service] = ch
	}
	e.mu.Unlock()

	e.queued.Add(1)
	defer e.queued.Add(-1)

	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *Engine) emit(req runRequest, state, detail string) {
	detail = req.detailPrefix + detail
	_ = e.opts.Journal.Progress(req.id, state, detail)
	e.notify(event(req, state, detail))
}

func (e *Engine) finish(req runRequest, state, detail string) {
	detail = req.detailPrefix + detail
	_ = e.opts.Journal.Finish(req.id, state, detail)
	e.notify(event(req, state, detail))
}

func event(req runRequest, state, detail string) Event {
	return Event{
		Service:   req.service,
		Action:    req.action,
		State:     state,
		Detail:    detail,
		JournalID: req.id,
		PRNumber:  req.prNumber,
		PRURL:     req.prURL,
		MergeURL:  req.mergeURL,
		At:        time.Now().UTC(),
	}
}

func (e *Engine) fail(req runRequest, detail string) {
	e.finish(req, StateFailed, detail)
}

func (e *Engine) notify(ev Event) {
	if e.opts.Notify != nil {
		e.opts.Notify(ev)
	}
}

func (e *Engine) closePR(ctx context.Context, prNumber int, comment string) {
	if err := e.opts.PR.ClosePR(ctx, prNumber, comment); err != nil {
		// Nothing to escalate to: the failure is already the journalled outcome.
		_ = err
	}
}

// setDigest rewrites the one digest occurrence in a manifest's bytes, leaving
// every comment and every other field exactly as the operator wrote them.
func setDigest(raw []byte, oldDigest, newDigest string) ([]byte, error) {
	count := bytes.Count(raw, []byte(oldDigest))
	if count != 1 {
		return nil, fmt.Errorf(
			"manifest contains the current digest %d times, expected exactly once", count)
	}
	return bytes.Replace(raw, []byte(oldDigest), []byte(newDigest), 1), nil
}

func shortDigest(d string) string {
	trimmed := strings.TrimPrefix(d, "sha256:")
	if len(trimmed) > 12 {
		return trimmed[:12]
	}
	return trimmed
}

func prTitle(action, service, digest string) string {
	verb := "deploy"
	if action == journal.ActionRollback {
		verb = "roll back"
	}
	return fmt.Sprintf("%s %s to %s", verb, service, shortDigest(digest))
}

func prBody(action, service, oldDigest, newDigest string) string {
	return fmt.Sprintf(
		"Authored by snowdeploy (%s).\n\n- service: `%s`\n- from: `%s`\n- to: `%s`\n\n"+
			"This change touches only the service manifest. The host applies it "+
			"after this pull request merges.",
		action, service, oldDigest, newDigest)
}
