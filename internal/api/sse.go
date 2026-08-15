package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/deploy"
	"github.com/SnowballSH/snowdeploy/internal/journal"
)

// subscriberBuffer is how far behind a slow reader may fall before it is
// dropped. Progress events must never block the deploy that produces them.
const subscriberBuffer = 64

// keepAlive keeps proxies from closing an idle stream.
const keepAlive = 25 * time.Second

// subscriber is one connected stream. warned makes the fell-behind log line
// fire once per connection: a reader that dropped one event has usually
// dropped hundreds, and one line per drop would bury the log.
type subscriber struct {
	ch     chan deploy.Event
	warned bool
}

// broker fans deploy events out to every connected UI and CLI.
type broker struct {
	mu   sync.Mutex
	next int64
	subs map[int64]*subscriber

	// onDrop counts every event a subscriber lost. Without it a dropped
	// event vanished without trace anywhere: the UI simply showed a stale
	// state and nothing recorded that the stream had lied.
	onDrop func()
}

func newBroker(onDrop func()) *broker {
	return &broker{subs: make(map[int64]*subscriber), onDrop: onDrop}
}

func (b *broker) subscribe() (<-chan deploy.Event, func()) {
	sub := &subscriber{ch: make(chan deploy.Event, subscriberBuffer)}

	b.mu.Lock()
	b.next++
	id := b.next
	b.subs[id] = sub
	b.mu.Unlock()

	return sub.ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if sub, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(sub.ch)
		}
	}
}

// publish never blocks: a subscriber that cannot keep up loses events rather
// than stalling the engine.
func (b *broker) publish(ev deploy.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, sub := range b.subs {
		select {
		case sub.ch <- ev:
		default:
			if !sub.warned {
				sub.warned = true
				slog.Warn("event subscriber fell behind; dropping events",
					"subscriber", id, "service", ev.Service)
			}
			if b.onDrop != nil {
				b.onDrop()
			}
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

	// The subscription is live before the snapshot is read, so a transition
	// landing in between arrives as a regular frame rather than being lost;
	// at worst the client sees the same state twice, which is idempotent.
	var seq int64
	writeFrame := func(ev deploy.Event) bool {
		payload, err := json.Marshal(ev)
		if err != nil {
			return true
		}
		seq++
		if _, err := fmt.Fprintf(w, "id: %d-%d\nevent: state\ndata: %s\n\n",
			ev.JournalID, seq, payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, ev := range s.snapshotEvents() {
		if !writeFrame(ev) {
			return
		}
	}

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
			if !writeFrame(ev) {
				return
			}
		}
	}
}

// snapshotEvents is what a fresh connection is owed before any live frame:
// one synthetic event per service whose newest journal row is still in
// flight. Without it a page opened mid-deploy showed nothing until the next
// transition happened to arrive.
func (s *Server) snapshotEvents() []deploy.Event {
	names, err := s.opts.Repo.Services()
	if err != nil {
		return nil
	}
	var out []deploy.Event
	for _, name := range names {
		recent, err := s.opts.History.Recent(name, 1)
		if err != nil || len(recent) == 0 || !recent[0].FinishedAt.IsZero() {
			continue
		}
		out = append(out, s.eventFromEntry(recent[0]))
	}
	return out
}

// eventFromEntry rebuilds the event the row's newest transition would have
// carried, links included, so a snapshot frame is indistinguishable from the
// live frame the client missed.
func (s *Server) eventFromEntry(e journal.Entry) deploy.Event {
	ev := deploy.Event{
		Service:   e.Service,
		Action:    e.Action,
		State:     e.State,
		Detail:    e.Detail,
		JournalID: e.ID,
		PRNumber:  e.PRNumber,
		At:        e.StartedAt,
	}
	if s.opts.RepoWebURL != "" {
		if e.PRNumber > 0 {
			ev.PRURL = s.opts.RepoWebURL + "/pull/" + strconv.Itoa(e.PRNumber)
		}
		if e.MergeSHA != "" {
			ev.MergeURL = s.opts.RepoWebURL + "/commit/" + e.MergeSHA
		}
	}
	return ev
}
