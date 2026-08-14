package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/deploy"
)

// subscriberBuffer is how far behind a slow reader may fall before it is
// dropped. Progress events must never block the deploy that produces them.
const subscriberBuffer = 64

// keepAlive keeps proxies from closing an idle stream.
const keepAlive = 25 * time.Second

// broker fans deploy events out to every connected UI and CLI.
type broker struct {
	mu   sync.Mutex
	next int64
	subs map[int64]chan deploy.Event
}

func newBroker() *broker {
	return &broker{subs: make(map[int64]chan deploy.Event)}
}

func (b *broker) subscribe() (<-chan deploy.Event, func()) {
	ch := make(chan deploy.Event, subscriberBuffer)

	b.mu.Lock()
	b.next++
	id := b.next
	b.subs[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if sub, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(sub)
		}
	}
}

// publish never blocks: a subscriber that cannot keep up loses events rather
// than stalling the engine.
func (b *broker) publish(ev deploy.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, unsubscribe := s.broker.subscribe()
	defer unsubscribe()

	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
