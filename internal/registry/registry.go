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

// Watcher polls a changing set of repositories and remembers the newest digest
// each one resolved to. A failed poll leaves the previous answer standing:
// a registry outage must never look like "the image disappeared".
type Watcher struct {
	resolver Resolver
	interval time.Duration
	tag      string

	mu     sync.RWMutex
	latest map[string]string
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
		if err != nil {
			continue
		}
		w.mu.Lock()
		w.latest[repo] = digest
		w.mu.Unlock()
	}
}
