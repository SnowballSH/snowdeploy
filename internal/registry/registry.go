// Package registry watches container registries for the digest a tag
// currently resolves to, so the operator can be offered a deploy without the
// daemon ever needing push credentials.
package registry

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// DefaultTag is the tag a watcher resolves when none is configured.
const DefaultTag = "latest"

// Resolver turns a repository and tag into an immutable digest.
type Resolver interface {
	ResolveDigest(ctx context.Context, repository, tag string) (string, error)
}

type ociResolver struct{}

// NewOCI resolves against any registry speaking the OCI distribution API,
// anonymously — public images only, and no credential to leak.
func NewOCI() Resolver { return ociResolver{} }

func (ociResolver) ResolveDigest(ctx context.Context, repository, tag string) (string, error) {
	ref, err := name.NewTag(repository + ":" + tag)
	if err != nil {
		return "", fmt.Errorf("parse reference %s:%s: %w", repository, tag, err)
	}
	desc, err := remote.Head(ref, remote.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("head %s: %w", ref, err)
	}
	return desc.Digest.String(), nil
}

// PollObserver is told the outcome of every resolve, successful or not.
//
// Without it a watcher that can no longer reach the registry is
// indistinguishable from one with nothing new to offer: the previous digest
// keeps being served, and nothing anywhere records that the question stopped
// being answered. It is a report, not a dependency — a watcher without one
// polls exactly the same.
type PollObserver func(repository string, err error)

// Watcher polls a changing set of repositories and remembers the newest digest
// each one resolved to. A failed poll leaves the previous answer standing:
// a registry outage must never look like "the image disappeared".
type Watcher struct {
	resolver Resolver
	interval time.Duration
	tag      string

	mu       sync.RWMutex
	latest   map[string]string
	observer PollObserver
}

// NewWatcher builds a watcher polling every interval.
func NewWatcher(r Resolver, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Watcher{
		resolver: r,
		interval: interval,
		tag:      DefaultTag,
		latest:   make(map[string]string),
	}
}

// SetObserver installs the poll observer. Call it before Run.
func (w *Watcher) SetObserver(fn PollObserver) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.observer = fn
}

// Latest reports the digest last seen for a repository.
func (w *Watcher) Latest(repository string) (string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	d, ok := w.latest[repository]
	return d, ok
}

// All returns a copy of every digest currently known.
func (w *Watcher) All() map[string]string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return maps.Clone(w.latest)
}

// Run polls until ctx is done. repositories is read every tick so a manifest
// change is picked up without a restart.
func (w *Watcher) Run(ctx context.Context, repositories func() []string) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.poll(ctx, repositories())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll(ctx, repositories())
		}
	}
}

func (w *Watcher) poll(ctx context.Context, repositories []string) {
	for _, repo := range repositories {
		if ctx.Err() != nil {
			return
		}
		digest, err := w.resolver.ResolveDigest(ctx, repo, w.tag)
		if err == nil {
			w.mu.Lock()
			w.latest[repo] = digest
			w.mu.Unlock()
		}
		// Stored before reported, so an observer that turns around and asks
		// Latest sees the digest this very poll resolved.
		w.report(repo, err)
	}
}

func (w *Watcher) report(repository string, err error) {
	w.mu.RLock()
	observer := w.observer
	w.mu.RUnlock()
	if observer != nil {
		observer(repository, err)
	}
}
