// Package revision resolves a pinned image digest back to the source commit
// that produced it, read from the provenance labels a build pipeline stamps
// into the image config. It exists so an operator looking at three digests
// sees three changes, not three hashes.
package revision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// The OCI image spec's standard provenance labels.
const (
	revisionLabel = "org.opencontainers.image.revision"
	sourceLabel   = "org.opencontainers.image.source"
)

const (
	// subjectLimit bounds the commit subject, so one pathological commit
	// message cannot bloat every status response that quotes it.
	subjectLimit = 100

	// subjectTimeout bounds the unauthenticated GitHub call. The subject is
	// decoration; a slow api.github.com must never stall a status request.
	subjectTimeout = 5 * time.Second

	// failureRetry is how long a failed lookup keeps answering "unknown"
	// before the registry is asked again. Long enough that a broken image is
	// not hammered on every status poll, short enough that a registry blip
	// does not blind the daemon until a restart.
	failureRetry = 10 * time.Minute

	// fillTimeout bounds one fill against the registry and GitHub combined.
	// Fills run detached from the caller's context — a caller's deadline says
	// how long it will wait, not how long the answer may take to compute — so
	// the fill needs a bound of its own.
	fillTimeout = 15 * time.Second

	// maxEntries bounds the cache. Four services' worth of digests cannot
	// legitimately approach this, so crossing it means the keys are garbage —
	// and then dropping the whole map is cheaper than choosing survivors.
	maxEntries = 512
)

// Revision is what is known about the commit behind a digest. Only SHA is
// required; URL and Subject are best-effort decoration.
type Revision struct {
	SHA     string `json:"sha"`
	URL     string `json:"url"`
	Subject string `json:"subject"`
}

// Source resolves a pinned image to its revision. Lookup reports false only
// when even the commit SHA is unknown: a missing source label or a failed
// subject fetch degrades the answer, it does not withhold it.
type Source interface {
	Lookup(ctx context.Context, repository, digest string) (Revision, bool)
}

// labelFetcher reads the config labels of repository@digest. It is a seam so
// tests never touch a registry.
type labelFetcher func(ctx context.Context, repository, digest string) (map[string]string, error)

// NewOCI resolves against any registry speaking the OCI distribution API,
// anonymously — the same posture as the registry watcher: public images only,
// and no credential to leak.
func NewOCI() Source {
	return newCached(fetchConfigLabels,
		&http.Client{Timeout: subjectTimeout}, "https://api.github.com")
}

func fetchConfigLabels(ctx context.Context, repository, digest string) (map[string]string, error) {
	ref, err := name.NewDigest(repository + "@" + digest)
	if err != nil {
		return nil, fmt.Errorf("parse reference %s@%s: %w", repository, digest, err)
	}
	img, err := remote.Image(ref, remote.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetch image %s: %w", ref, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read image config of %s: %w", ref, err)
	}
	return cfg.Config.Labels, nil
}

type cacheEntry struct {
	rev Revision
	ok  bool
	at  time.Time
}

// cachedSource remembers every answer. A successful lookup is immutable — a
// digest's provenance cannot change — so it is kept forever; a failed one is
// retried after failureRetry.
//
// Misses are filled by one detached goroutine per key. Detached, because the
// caller is typically an HTTP handler whose deadline bounds its wait, not the
// work: letting an impatient caller abort the fill would also let it
// negative-cache an answer every patient caller still wants. One goroutine
// per key, because a status page asks about the same digest from several rows
// at once, and a thundering herd of anonymous pulls is how a registry decides
// to rate-limit the daemon.
type cachedSource struct {
	fetch     labelFetcher
	github    *http.Client
	githubAPI string
	now       func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
	fills   map[string]chan struct{}
}

func newCached(fetch labelFetcher, github *http.Client, githubAPI string) *cachedSource {
	return &cachedSource{
		fetch:     fetch,
		github:    github,
		githubAPI: strings.TrimSuffix(githubAPI, "/"),
		now:       time.Now,
		entries:   make(map[string]cacheEntry),
		fills:     make(map[string]chan struct{}),
	}
}

// Lookup answers from the cache, joining or starting a background fill on a
// miss. It gives up with (Revision{}, false) as soon as ctx is done; the fill
// runs on regardless and caches whatever it learns for the next caller.
func (s *cachedSource) Lookup(ctx context.Context, repository, digest string) (Revision, bool) {
	key := repository + "@" + digest
	for {
		rev, ok, decided, done := s.cachedOrFilling(key, repository, digest)
		if decided {
			return rev, ok
		}
		select {
		case <-done:
			// Loop rather than trust the fill's outcome blindly: re-reading
			// the cache keeps this correct even if the entry was dropped in a
			// wholesale reset meanwhile.
		case <-ctx.Done():
			return Revision{}, false
		}
	}
}

// cachedOrFilling answers from the cache, or hands back the in-flight fill for
// the caller to wait on — starting one when nobody has. decided=false means
// wait on done.
func (s *cachedSource) cachedOrFilling(
	key, repository, digest string,
) (rev Revision, ok, decided bool, done chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, present := s.entries[key]; present {
		if e.ok {
			return e.rev, true, true, nil
		}
		if s.now().Sub(e.at) < failureRetry {
			return Revision{}, false, true, nil
		}
	}
	done, filling := s.fills[key]
	if !filling {
		done = make(chan struct{})
		s.fills[key] = done
		go s.fill(key, repository, digest, done)
	}
	return Revision{}, false, false, done
}

// fill resolves one key under its own bounded context and publishes the
// outcome. Only its own failure negative-caches; no caller can abort it.
func (s *cachedSource) fill(key, repository, digest string, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), fillTimeout)
	defer cancel()
	rev, ok := s.resolve(ctx, repository, digest)

	s.mu.Lock()
	s.store(key, rev, ok)
	delete(s.fills, key)
	s.mu.Unlock()
	close(done)
}

// store records an outcome. It must be called with s.mu held.
func (s *cachedSource) store(key string, rev Revision, ok bool) {
	if len(s.entries) >= maxEntries {
		s.entries = make(map[string]cacheEntry)
	}
	s.entries[key] = cacheEntry{rev: rev, ok: ok, at: s.now()}
}

// resolve reads the provenance labels and, when the source lives on
// github.com, decorates the revision with the commit subject.
func (s *cachedSource) resolve(ctx context.Context, repository, digest string) (Revision, bool) {
	labels, err := s.fetch(ctx, repository, digest)
	if err != nil {
		return Revision{}, false
	}
	sha := labels[revisionLabel]
	if sha == "" {
		return Revision{}, false
	}
	rev := Revision{SHA: sha}

	// The label convention is a web URL, but ".git" clone-URL spellings exist
	// in the wild and would corrupt both the commit link and the API path.
	source := strings.TrimSuffix(strings.TrimSuffix(labels[sourceLabel], "/"), ".git")
	if source == "" {
		return rev, true
	}
	rev.URL = source + "/commit/" + sha
	if owner, repo, ok := githubRepository(source); ok {
		rev.Subject = s.commitSubject(ctx, owner, repo, sha)
	}
	return rev, true
}

// githubRepository extracts owner and repository from a github.com web URL.
// Any other forge still gets a commit URL, just no subject: there is no API
// this daemon knows how to ask.
func githubRepository(source string) (owner, repo string, ok bool) {
	u, err := url.Parse(source)
	if err != nil || u.Host != "github.com" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// commitSubject asks the GitHub API, unauthenticated, for the commit message
// and keeps its first line. Every failure returns "": the subject rides along
// on a lookup it must never be able to fail.
func (s *cachedSource) commitSubject(ctx context.Context, owner, repo, sha string) string {
	ctx, cancel := context.WithTimeout(ctx, subjectTimeout)
	defer cancel()

	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s", s.githubAPI, owner, repo, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := s.github.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var body struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	subject, _, _ := strings.Cut(body.Commit.Message, "\n")
	subject = strings.TrimSuffix(subject, "\r")
	if runes := []rune(subject); len(runes) > subjectLimit {
		subject = string(runes[:subjectLimit])
	}
	return subject
}
