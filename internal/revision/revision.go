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
type cachedSource struct {
	fetch     labelFetcher
	github    *http.Client
	githubAPI string
	now       func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

func newCached(fetch labelFetcher, github *http.Client, githubAPI string) *cachedSource {
	return &cachedSource{
		fetch:     fetch,
		github:    github,
		githubAPI: strings.TrimSuffix(githubAPI, "/"),
		now:       time.Now,
		entries:   make(map[string]cacheEntry),
	}
}

func (s *cachedSource) Lookup(ctx context.Context, repository, digest string) (Revision, bool) {
	key := repository + "@" + digest
	if rev, ok, decided := s.cached(key); decided {
		return rev, ok
	}
	rev, ok := s.resolve(ctx, repository, digest)
	s.store(key, rev, ok)
	return rev, ok
}

// cached answers from the cache; decided=false means the caller must resolve.
func (s *cachedSource) cached(key string) (rev Revision, ok, decided bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, present := s.entries[key]
	switch {
	case !present:
		return Revision{}, false, false
	case e.ok:
		return e.rev, true, true
	case s.now().Sub(e.at) < failureRetry:
		return Revision{}, false, true
	default:
		return Revision{}, false, false
	}
}

func (s *cachedSource) store(key string, rev Revision, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
