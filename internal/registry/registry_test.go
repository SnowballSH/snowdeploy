package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeResolver struct {
	mu      sync.Mutex
	digests map[string]string
	err     error
	calls   int
}

func (f *fakeResolver) ResolveDigest(_ context.Context, repository, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	d, ok := f.digests[repository]
	if !ok {
		return "", errors.New("no such repository")
	}
	return d, nil
}

func (f *fakeResolver) set(repo, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digests[repo] = digest
}

func (f *fakeResolver) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestWatcherPopulatesLatestOnFirstTick(t *testing.T) {
	f := &fakeResolver{digests: map[string]string{
		"registry.example.com/acme/web": "sha256:aaa",
		"registry.example.com/acme/api": "sha256:bbb",
	}}
	w := NewWatcher(f, time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	go w.Run(ctx, func() []string {
		return []string{"registry.example.com/acme/web", "registry.example.com/acme/api"}
	})

	waitFor(t, func() bool {
		a, okA := w.Latest("registry.example.com/acme/web")
		b, okB := w.Latest("registry.example.com/acme/api")
		return okA && okB && a == "sha256:aaa" && b == "sha256:bbb"
	})
}

func TestLatestUnknownRepository(t *testing.T) {
	w := NewWatcher(&fakeResolver{digests: map[string]string{}}, time.Hour)
	if _, ok := w.Latest("registry.example.com/acme/nope"); ok {
		t.Fatal("unknown repository reported ok")
	}
}

func TestResolverErrorKeepsPreviousValue(t *testing.T) {
	f := &fakeResolver{digests: map[string]string{"repo": "sha256:good"}}
	w := NewWatcher(f, time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	go w.Run(ctx, func() []string { return []string{"repo"} })

	waitFor(t, func() bool {
		d, ok := w.Latest("repo")
		return ok && d == "sha256:good"
	})

	f.fail(errors.New("registry down"))
	before := f.calls
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.calls > before+2
	})

	d, ok := w.Latest("repo")
	if !ok || d != "sha256:good" {
		t.Fatalf("Latest after resolver error = %q, %v; want the previous value", d, ok)
	}
}

func TestWatcherPicksUpNewDigest(t *testing.T) {
	f := &fakeResolver{digests: map[string]string{"repo": "sha256:one"}}
	w := NewWatcher(f, time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	go w.Run(ctx, func() []string { return []string{"repo"} })

	waitFor(t, func() bool {
		d, _ := w.Latest("repo")
		return d == "sha256:one"
	})

	f.set("repo", "sha256:two")
	waitFor(t, func() bool {
		d, _ := w.Latest("repo")
		return d == "sha256:two"
	})
}

func TestRunStopsOnContextCancel(t *testing.T) {
	f := &fakeResolver{digests: map[string]string{"repo": "sha256:one"}}
	w := NewWatcher(f, time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, func() []string { return []string{"repo"} })
		close(done)
	}()

	waitFor(t, func() bool { _, ok := w.Latest("repo"); return ok })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestAllReturnsSnapshot(t *testing.T) {
	f := &fakeResolver{digests: map[string]string{"repo": "sha256:one"}}
	w := NewWatcher(f, time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	go w.Run(ctx, func() []string { return []string{"repo"} })

	waitFor(t, func() bool { _, ok := w.Latest("repo"); return ok })

	snap := w.All()
	if snap["repo"] != "sha256:one" {
		t.Fatalf("All() = %v", snap)
	}
	snap["repo"] = "mutated"
	if d, _ := w.Latest("repo"); d != "sha256:one" {
		t.Fatal("All() returned the live map, not a copy")
	}
}

// A watcher whose every poll fails looks, from outside, exactly like a watcher
// with nothing new to offer: Latest keeps answering the previous digest and no
// caller learns the registry went away. These two prove the outcome of every
// poll is reported, so the daemon can log it and count it.
func TestObserverIsToldEveryFailedPoll(t *testing.T) {
	r := &fakeResolver{digests: map[string]string{}}
	r.fail(errors.New("registry unreachable"))
	w := NewWatcher(r, time.Millisecond)

	var mu sync.Mutex
	failures := map[string]string{}
	w.SetObserver(func(repository string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failures[repository] = err.Error()
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx, func() []string { return []string{"repo"} })

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return failures["repo"] != ""
	})
	mu.Lock()
	defer mu.Unlock()
	if got := failures["repo"]; got == "" {
		t.Fatal("a failing poll was never reported")
	}
}

func TestObserverIsToldEverySuccessfulPoll(t *testing.T) {
	r := &fakeResolver{digests: map[string]string{"repo": "sha256:one"}}
	w := NewWatcher(r, time.Millisecond)

	var mu sync.Mutex
	var successes int
	var sawError bool
	w.SetObserver(func(_ string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			sawError = true
			return
		}
		successes++
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx, func() []string { return []string{"repo"} })

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return successes > 0
	})
	mu.Lock()
	defer mu.Unlock()
	if sawError {
		t.Error("a successful poll was reported as an error")
	}
}

// A watcher with no observer must still poll: the observer is a report, not a
// dependency.
func TestWatcherPollsWithoutAnObserver(t *testing.T) {
	r := &fakeResolver{digests: map[string]string{"repo": "sha256:one"}}
	w := NewWatcher(r, time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx, func() []string { return []string{"repo"} })

	waitFor(t, func() bool { _, ok := w.Latest("repo"); return ok })
}
