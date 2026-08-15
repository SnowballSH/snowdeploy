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

// ConfigRepo is the merged-state mirror the engine reads.
type ConfigRepo interface {
	Sync(ctx context.Context) (string, error)
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

	mu    sync.Mutex
	locks map[string]chan struct{}

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
	return &Engine{
		opts:    opts,
		baseCtx: ctx,
		locks:   make(map[string]chan struct{}),
	}
}

// Wait blocks until every in-flight deploy has finished.
func (e *Engine) Wait() { e.inFlight.Wait() }

// QueueDepth is the number of requests waiting on a per-service lock.
func (e *Engine) QueueDepth() int64 { return e.queued.Load() }

// Deploy pins a service to a digest. It returns as soon as the journal entry
// exists; progress arrives as events.
func (e *Engine) Deploy(ctx context.Context, service, digest, actor string) (int64, error) {
	if !digestPattern.MatchString(digest) {
		return 0, fmt.Errorf("%q is not a sha256 digest pin", digest)
	}
	return e.start(ctx, journal.ActionDeploy, service, actor,
		func(string) (string, error) { return digest, nil })
}

// Rollback re-pins a service to a previous digest. With no explicit target it
// uses the newest healthy digest that is not the one currently pinned — the
// digest the operator means by "roll back", not the one already running.
func (e *Engine) Rollback(ctx context.Context, service, toDigest, actor string) (int64, error) {
	if toDigest != "" && !digestPattern.MatchString(toDigest) {
		return 0, fmt.Errorf("%q is not a sha256 digest pin", toDigest)
	}
	return e.start(ctx, journal.ActionRollback, service, actor,
		func(current string) (string, error) {
			if toDigest != "" {
				return toDigest, nil
			}
			return e.opts.Journal.PreviousHealthyDigest(service, current)
		})
}

// resolveTarget turns a request into the digest to pin, given what is pinned.
type resolveTarget func(currentDigest string) (string, error)

func (e *Engine) start(
	ctx context.Context, action, service, actor string, resolve resolveTarget,
) (int64, error) {
	if _, err := e.opts.Repo.Sync(ctx); err != nil {
		return 0, fmt.Errorf("sync configuration repository: %w", err)
	}
	current, raw, err := e.opts.Repo.Manifest(service)
	if err != nil {
		return 0, err
	}

	digest, err := resolve(current.Image.Digest)
	if err != nil {
		return 0, err
	}
	if !digestPattern.MatchString(digest) {
		return 0, fmt.Errorf("%q is not a sha256 digest pin", digest)
	}
	if current.Image.Digest == digest {
		return 0, fmt.Errorf("%s: %w (%s)", service, ErrAlreadyAtDigest, digest)
	}

	newContent, err := setDigest(raw, current.Image.Digest, digest)
	if err != nil {
		return 0, err
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
		return 0, err
	}

	e.inFlight.Add(1)
	go func() {
		defer e.inFlight.Done()
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
	return id, nil
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
}

func (e *Engine) run(req runRequest) {
	ctx := e.baseCtx

	release, err := e.lock(ctx, req.service)
	if err != nil {
		e.fail(req, fmt.Sprintf("queued deploy abandoned: %v", err))
		return
	}
	defer release()

	e.mergeMu.Lock()
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
			return "", errors.New(detail)
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
			return "", fmt.Errorf("merge failed (%v) and the branch could not be updated: %w",
				err, updateErr)
		}
	}
	return "", fmt.Errorf("merge failed after %d attempts: %w", mergeAttempts, lastErr)
}

// applyMerged pulls the merged branch and walks it onto the host.
func (e *Engine) applyMerged(ctx context.Context, req runRequest) error {
	if _, err := e.opts.Repo.Sync(ctx); err != nil {
		return fmt.Errorf("sync merged state: %w", err)
	}
	m, _, err := e.opts.Repo.Manifest(req.service)
	if err != nil {
		return err
	}
	if m.Image.Digest != req.newDigest {
		return fmt.Errorf(
			"merged manifest pins %s, not the requested %s", m.Image.Digest, req.newDigest)
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

// rollback restores the previously running digest on the host, then brings
// main back in line with it so the record keeps describing what runs.
func (e *Engine) rollback(ctx context.Context, req runRequest, cause error) {
	if restoreErr := e.restorePrevious(ctx, req); restoreErr != nil {
		e.finish(req, StateFailed, fmt.Sprintf(
			"%v; the automatic rollback also failed: %v", cause, restoreErr))
		return
	}

	detail := fmt.Sprintf("%v; rolled back to %s", cause, req.oldDigest)
	if err := e.revertMain(ctx, req); err != nil {
		detail += fmt.Sprintf("; main still pins %s and needs a manual revert (%v)",
			req.newDigest, err)
	} else {
		detail += "; main reverted"
	}
	e.finish(req, StateRolledBack, detail)
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
	return e.opts.Applier.Apply(ctx, m, tmpl, nil)
}

// revertMain opens and merges a pull request restoring the previous manifest,
// retrying a bounded number of times before giving up loudly.
func (e *Engine) revertMain(ctx context.Context, req runRequest) error {
	var last error
	for attempt := 1; attempt <= e.opts.RevertAttempts; attempt++ {
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

func (e *Engine) revertOnce(ctx context.Context, req runRequest) error {
	e.mergeMu.Lock()
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
	if _, err := e.mergeThroughChecks(ctx, req, prNumber); err != nil {
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
	_ = e.opts.Journal.Progress(req.id, state, detail)
	e.notify(event(req, state, detail))
}

func (e *Engine) finish(req runRequest, state, detail string) {
	_ = e.opts.Journal.Finish(req.id, state, detail)
	e.notify(event(req, state, detail))
}

func event(req runRequest, state, detail string) Event {
	return Event{
		Service:   req.service,
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
