// Package api is the daemon's HTTP surface: a small JSON API, a live event
// stream, and a separate Prometheus listener. Authentication is either a
// bearer token whose hash the operator placed on disk, or the identity a
// fronting proxy has already established.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/deploy"
	"github.com/SnowballSH/snowdeploy/internal/journal"
	"github.com/SnowballSH/snowdeploy/internal/manifest"
	"github.com/SnowballSH/snowdeploy/internal/registry"
	"github.com/SnowballSH/snowdeploy/internal/revision"
)

// defaultHistory bounds an unqualified history request.
const defaultHistory = 20

// maxHistory bounds a caller-supplied one.
const maxHistory = 200

// maxRevisionDigests bounds one revisions request. A service page legitimately
// asks about a screenful of history at once, never hundreds.
const maxRevisionDigests = 40

// statusRevisionBudget is how long one status row waits for its revision
// lookups, combined. Cache misses keep filling in the background past this
// deadline, so a cold listing answers fast with whatever is warm and the rest
// is there on the next refresh — rather than every browser paying a registry
// round-trip to render a table.
const statusRevisionBudget = 1500 * time.Millisecond

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Engine is the deploy machinery the API drives. The bool is joined: the
// request matched a run already carrying the same digest and was answered
// with its journal id instead of starting a second one.
type Engine interface {
	Deploy(ctx context.Context, service, digest, actor string) (int64, bool, error)
	Rollback(ctx context.Context, service, toDigest, actor string) (int64, bool, error)
	Converge(ctx context.Context, service, actor string) (int64, bool, error)
	Drift(ctx context.Context) (map[string]string, error)
	QueueDepth() int64
}

// Repo is the merged-manifest view.
type Repo interface {
	Sync(ctx context.Context) (string, error)
	Services() ([]string, error)
	Manifest(service string) (*manifest.Manifest, []byte, error)
	// SyncState reports whether the mirror has ever been populated and, when
	// the last attempt failed, a reason the repository authored itself. The
	// API forwards that reason to clients and never the error from Sync.
	SyncState() (synced bool, reason error)
}

// Inspector reports the digest actually running.
type Inspector interface {
	RunningImage(ctx context.Context, service string) (string, error)
}

// Watcher reports the newest digest a registry offers, and how the last
// attempt to ask went.
type Watcher interface {
	Latest(repository string) (string, bool)
	LastPoll(repository string) (registry.PollStatus, bool)
}

// History reads deploy receipts.
type History interface {
	Recent(service string, n int) ([]journal.Entry, error)
}

// ServiceStatus is one row of the service list. Revisions maps each of the
// row's digests to the commit that built it, and only carries the ones a
// lookup actually answered.
type ServiceStatus struct {
	Name            string                       `json:"name"`
	Repository      string                       `json:"repository"`
	ManifestDigest  string                       `json:"manifestDigest"`
	RunningDigest   string                       `json:"runningDigest"`
	LatestAvailable string                       `json:"latestAvailable"`
	Drifted         bool                         `json:"drifted"`
	LastDeploy      *journal.Entry               `json:"lastDeploy,omitempty"`
	Revisions       map[string]revision.Revision `json:"revisions,omitempty"`

	// RegistryReachable distinguishes "no new image" from "no registry":
	// without it a registry outage renders as nothing new to offer, forever.
	// It is meaningful only when LatestCheckedAt is set — before the first
	// poll completes there is no verdict either way.
	RegistryReachable bool      `json:"registryReachable"`
	LatestCheckedAt   time.Time `json:"latestCheckedAt,omitzero"`

	// RepoWebURL lets a client build pull-request and commit links for
	// journal-sourced rows the same way the event stream does.
	RepoWebURL string `json:"repoWebUrl,omitempty"`
}

// Options are the server's dependencies.
type Options struct {
	Engine           Engine
	Repo             Repo
	Inspector        Inspector
	Watcher          Watcher
	History          History
	CLITokenHashFile string
	UI               http.Handler

	// RepoWebURL is the configuration repository's web address, threaded to
	// clients and used to rebuild event links for snapshot frames.
	RepoWebURL string

	// Revisions resolves a digest to the commit that built it. Nil is allowed:
	// status rows and the revisions endpoint then simply omit revision data.
	Revisions revision.Source
}

// Server is the API. Publish feeds it the engine's events.
type Server struct {
	opts    Options
	auth    *authenticator
	broker  *broker
	metrics *metrics
}

// New builds the server.
func New(opts Options) *Server {
	s := &Server{
		opts: opts,
		auth: &authenticator{tokenHashFile: opts.CLITokenHashFile},
	}
	s.metrics = newMetrics(func() float64 {
		if opts.Engine == nil {
			return 0
		}
		return float64(opts.Engine.QueueDepth())
	})
	s.broker = newBroker(s.metrics.droppedEvents.Inc)
	return s
}

// Publish records an event and fans it out. It is the engine's Notify.
func (s *Server) Publish(ev deploy.Event) {
	s.metrics.observe(ev)
	s.broker.publish(ev)
}

// SetDrift republishes the drift gauges.
func (s *Server) SetDrift(drift map[string]string) {
	known, err := s.opts.Repo.Services()
	if err != nil {
		known = nil
	}
	s.metrics.setDrift(drift, known)
}

// RecordRegistryPoll counts one registry resolution. It is the watcher's
// PollObserver, and the only place a registry the daemon can no longer reach
// becomes visible to anything but a log line.
func (s *Server) RecordRegistryPoll(repository string, err error) {
	s.metrics.recordRegistryPoll(repository, err)
}

// Handler is the loopback API and, when configured, the embedded UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.Handle("GET /api/v1/services", s.authed(s.handleServices))
	mux.Handle("GET /api/v1/services/{name}/history", s.authed(s.handleHistory))
	mux.Handle("GET /api/v1/services/{name}/revisions", s.authed(s.handleRevisions))
	mux.Handle("POST /api/v1/services/{name}/deploy", s.authedFor(actionDeploy, s.handleDeploy))
	mux.Handle("POST /api/v1/services/{name}/rollback", s.authedFor(actionRollback, s.handleRollback))
	mux.Handle("POST /api/v1/services/{name}/converge", s.authedFor(actionConverge, s.handleConverge))
	mux.Handle("GET /api/v1/events", s.authed(func(w http.ResponseWriter, r *http.Request, _ string) {
		s.handleEvents(w, r)
	}))

	if s.opts.UI != nil {
		mux.Handle("/", s.opts.UI)
	}
	return mux
}

// MetricsHandler is the separate scrape listener. It carries no deploy API:
// a scrape target must never be a control plane.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", s.metrics.handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// actorHandler is a handler that has already been given an authenticated actor.
type actorHandler func(w http.ResponseWriter, r *http.Request, actor string)

func (s *Server) authed(h actorHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, _, ok := s.identify(w, r)
		if !ok {
			return
		}
		h(w, r, actor)
	})
}

// authedFor guards a mutating route: the identity must also be allowed to take
// that action, so a token issued for converge alone cannot move a pin.
func (s *Server) authedFor(act action, h actorHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, allowed, ok := s.identify(w, r)
		if !ok {
			return
		}
		if !allowed.allows(act) {
			writeError(w, http.StatusForbidden,
				fmt.Errorf("this token may not %s", act))
			return
		}
		h(w, r, actor)
	})
}

func (s *Server) identify(w http.ResponseWriter, r *http.Request) (string, scope, bool) {
	actor, allowed, ok := s.auth.actor(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("no authenticated identity"))
		return "", nil, false
	}
	return actor, allowed, true
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request, _ string) {
	if _, err := s.opts.Repo.Sync(r.Context()); err != nil {
		// The error itself stops at the daemon's own journal, where the
		// identical start-up failure is already logged. It must not travel
		// further: a Git transport error quotes the remote URL, and an
		// operator who ever wrote a token into that URL would be handing it
		// to every browser and CLI behind the proxy. What crosses the wire is
		// the repository's own reason.
		slog.Warn("configuration sync failed while listing services", "error", err)
		synced, reason := s.opts.Repo.SyncState()
		writeUnavailable(w, synced, reason)
		return
	}
	names, err := s.opts.Repo.Services()
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	out := make([]ServiceStatus, 0, len(names))
	for _, name := range names {
		out = append(out, s.status(r.Context(), name))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) status(ctx context.Context, name string) ServiceStatus {
	st := ServiceStatus{Name: name}

	m, _, err := s.opts.Repo.Manifest(name)
	if err != nil {
		return st
	}
	st.Repository = m.Image.Repository
	st.ManifestDigest = m.Image.Digest

	if running, err := s.opts.Inspector.RunningImage(ctx, name); err == nil {
		st.RunningDigest = running
	}
	st.Drifted = st.RunningDigest != st.ManifestDigest

	if latest, ok := s.opts.Watcher.Latest(m.Image.Repository); ok {
		st.LatestAvailable = latest
	}
	if poll, ok := s.opts.Watcher.LastPoll(m.Image.Repository); ok {
		st.RegistryReachable = poll.Err == nil
		st.LatestCheckedAt = poll.At
	}
	st.RepoWebURL = s.opts.RepoWebURL
	if recent, err := s.opts.History.Recent(name, 1); err == nil && len(recent) > 0 {
		entry := recent[0]
		st.LastDeploy = &entry
	}
	revCtx, cancel := context.WithTimeout(ctx, statusRevisionBudget)
	defer cancel()
	st.Revisions = s.resolveRevisions(revCtx, st.Repository,
		[]string{st.ManifestDigest, st.RunningDigest, st.LatestAvailable})
	return st
}

// resolveRevisions answers what is known about each digest, deduplicated. The
// lookups run concurrently — they share whatever deadline ctx carries, and
// three digests waiting in series would triple it. A digest nothing could be
// learned about is left out rather than carried as an empty object: absence is
// the honest answer, not a blank one.
func (s *Server) resolveRevisions(
	ctx context.Context, repository string, digests []string,
) map[string]revision.Revision {
	if s.opts.Revisions == nil {
		return nil
	}
	seen := make(map[string]bool, len(digests))
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[string]revision.Revision)
	)
	for _, digest := range digests {
		if digest == "" || seen[digest] {
			continue
		}
		seen[digest] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rev, ok := s.opts.Revisions.Lookup(ctx, repository, digest); ok {
				mu.Lock()
				out[digest] = rev
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, _ string) {
	name, ok := s.service(w, r)
	if !ok {
		return
	}

	n := defaultHistory
	if raw := r.URL.Query().Get("n"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, errors.New("n must be a positive integer"))
			return
		}
		n = min(parsed, maxHistory)
	}

	entries, err := s.opts.History.Recent(name, n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if entries == nil {
		entries = []journal.Entry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleRevisions maps the caller's digests to the commits that built them.
// It exists so the history view can annotate old digests without the /history
// response shape ever changing — a pinned CLI parses that one. Unknown digests
// are omitted from the answer, never errors: an unlabelled image is a normal
// image.
func (s *Server) handleRevisions(w http.ResponseWriter, r *http.Request, _ string) {
	name, ok := s.service(w, r)
	if !ok {
		return
	}

	raw := r.URL.Query().Get("digests")
	if raw == "" {
		writeError(w, http.StatusBadRequest, errors.New("digests is required"))
		return
	}
	digests := strings.Split(raw, ",")
	if len(digests) > maxRevisionDigests {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"at most %d digests per request", maxRevisionDigests))
		return
	}
	for _, digest := range digests {
		if !digestPattern.MatchString(digest) {
			writeError(w, http.StatusBadRequest, fmt.Errorf(
				"%q is not a sha256 digest", digest))
			return
		}
	}

	m, _, err := s.opts.Repo.Manifest(name)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("no such service"))
		return
	}
	out := s.resolveRevisions(r.Context(), m.Image.Repository, digests)
	if out == nil {
		out = map[string]revision.Revision{}
	}
	writeJSON(w, http.StatusOK, out)
}

type digestRequest struct {
	Digest string `json:"digest"`
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, actor string) {
	name, ok := s.service(w, r)
	if !ok {
		return
	}
	req, ok := decodeDigest(w, r)
	if !ok {
		return
	}
	if req.Digest == "" {
		writeError(w, http.StatusBadRequest, errors.New("digest is required"))
		return
	}
	s.dispatch(w, r, func() (int64, bool, error) {
		return s.opts.Engine.Deploy(r.Context(), name, req.Digest, actor)
	})
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request, actor string) {
	name, ok := s.service(w, r)
	if !ok {
		return
	}
	req, ok := decodeDigest(w, r)
	if !ok {
		return
	}
	s.dispatch(w, r, func() (int64, bool, error) {
		return s.opts.Engine.Rollback(r.Context(), name, req.Digest, actor)
	})
}

// handleConverge re-applies the merged manifest as it stands. It reads no
// body: converge has no digest to accept, and accepting one would reintroduce
// the same-digest ambiguity the action exists to end.
func (s *Server) handleConverge(w http.ResponseWriter, r *http.Request, actor string) {
	name, ok := s.service(w, r)
	if !ok {
		return
	}
	s.dispatch(w, r, func() (int64, bool, error) {
		return s.opts.Engine.Converge(r.Context(), name, actor)
	})
}

// accepted is the body of a 202: the journal id to follow, and whether the
// click joined a run already carrying the same digest instead of starting one.
type accepted struct {
	JournalID int64 `json:"journalId"`
	Joined    bool  `json:"joined"`
}

func (s *Server) dispatch(w http.ResponseWriter, _ *http.Request, run func() (int64, bool, error)) {
	id, joined, err := run()
	if err != nil {
		// An engine error that grew out of a failed sync embeds whatever the
		// Git transport said, remote URL included — the same leak
		// handleServices closes for reads. The client gets the repository's
		// own reason; everything else is a plain validation failure.
		if synced, reason := s.opts.Repo.SyncState(); !synced ||
			strings.Contains(err.Error(), "sync configuration repository:") {
			slog.Warn("deploy request failed on configuration sync", "error", err)
			writeUnavailable(w, synced, reason)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, accepted{JournalID: id, Joined: joined})
}

// service resolves and validates the path's service name against the merged
// manifests, so an unknown or crafted name never reaches the engine.
//
// Readiness is settled before the lookup, and that order is the point: a
// daemon with no copy of the configuration repository cannot distinguish a
// service that was deleted from one it has simply never read about, so it must
// not answer as though it could.
func (s *Server) service(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		writeError(w, http.StatusBadRequest, errors.New("invalid service name"))
		return "", false
	}
	if synced, reason := s.opts.Repo.SyncState(); !synced {
		writeUnavailable(w, synced, reason)
		return "", false
	}
	if _, _, err := s.opts.Repo.Manifest(name); err != nil {
		writeError(w, http.StatusNotFound, errors.New("no such service"))
		return "", false
	}
	return name, true
}

func decodeDigest(w http.ResponseWriter, r *http.Request) (digestRequest, bool) {
	var req digestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && err.Error() != "EOF" {
		writeError(w, http.StatusBadRequest, err)
		return digestRequest{}, false
	}
	return req, true
}

// errNoConfiguration answers a Repo that reports itself unsynced without
// saying why. That breaks the interface's contract, but a vague sentence beats
// dereferencing a nil error inside a handler.
var errNoConfiguration = errors.New(
	"the daemon has no usable copy of the configuration repository")

// writeUnavailable answers a request the daemon cannot honour because it has
// no usable view of the configuration repository. It is the one place that
// choice of status code lives.
//
// A mirror that was never populated is 503. The condition is temporary and
// operator-clearable — this host's secret store seals on every reboot, so the
// daemon routinely starts with no credential and therefore no clone — and 503
// says something about the server rather than about the resource, which is
// exactly the distinction being drawn: the daemon does not know whether the
// service exists. 404 asserts that it does not, which is a claim there is no
// evidence for, and it is the claim that sends an operator looking for a
// manifest nobody deleted. 500 would say something broke; nothing did, this
// start-up posture is deliberate. 502 would blame the remote for a key the
// host could not read locally. No Retry-After rides along: the delay is
// however long it takes a human to unseal the store, and a number here would
// be a promise the daemon cannot keep.
//
// A mirror that is populated but could not be refreshed is 502, because there
// the daemon really did act as a gateway to the remote and the remote is the
// half that did not answer.
func writeUnavailable(w http.ResponseWriter, synced bool, reason error) {
	if reason == nil {
		reason = errNoConfiguration
	}
	code := http.StatusServiceUnavailable
	if synced {
		code = http.StatusBadGateway
	}
	writeError(w, code, reason)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
