package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/deploy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics is the daemon's Prometheus surface. Per D036 nothing here pushes:
// an alert rule on these series is the whole notification path.
type metrics struct {
	registry      *prometheus.Registry
	deploys       *prometheus.CounterVec
	stateDuration *prometheus.HistogramVec
	drift         *prometheus.GaugeVec
	registryPolls *prometheus.CounterVec

	mu        sync.Mutex
	lastState map[int64]stateMark
}

type stateMark struct {
	state string
	at    time.Time
}

func newMetrics(queueDepth func() float64) *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		deploys: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "snowdeploy_deploys_total",
			Help: "Deploys by service and terminal outcome.",
		}, []string{"service", "outcome"}),
		stateDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "snowdeploy_state_duration_seconds",
			Help:    "Time each deploy spent in a state.",
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 12),
		}, []string{"state"}),
		drift: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "snowdeploy_drift",
			Help: "1 when a service's running image differs from its merged manifest.",
		}, []string{"service"}),
		// The outcome is a label, never the error text: a resolve failure can
		// carry a URL, and every scraper reads a label.
		registryPolls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "snowdeploy_registry_polls_total",
			Help: "Registry tag resolutions by repository and outcome.",
		}, []string{"repository", "outcome"}),
		lastState: make(map[int64]stateMark),
	}

	m.registry.MustRegister(
		m.deploys, m.stateDuration, m.drift, m.registryPolls,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "snowdeploy_queue_depth",
			Help: "Deploy requests waiting on a per-service lock.",
		}, queueDepth),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// observe turns the event stream into timing and outcome series.
func (m *metrics) observe(ev deploy.Event) {
	if ev.JournalID == 0 {
		return
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now().UTC()
	}

	m.mu.Lock()
	if prev, ok := m.lastState[ev.JournalID]; ok {
		m.stateDuration.WithLabelValues(prev.state).Observe(at.Sub(prev.at).Seconds())
	}
	if isTerminalState(ev.State) {
		delete(m.lastState, ev.JournalID)
	} else {
		m.lastState[ev.JournalID] = stateMark{state: ev.State, at: at}
	}
	m.mu.Unlock()

	if isTerminalState(ev.State) {
		m.deploys.WithLabelValues(ev.Service, ev.State).Inc()
	}
}

// recordRegistryPoll counts one resolve. A watcher that stops being able to
// answer otherwise looks exactly like a watcher with nothing new to offer, so
// this counter is the only thing that separates "no new image" from "no
// registry".
func (m *metrics) recordRegistryPoll(repository string, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	m.registryPolls.WithLabelValues(repository, outcome).Inc()
}

// setDrift republishes the whole drift picture, so a service that stops
// drifting drops back to 0 instead of keeping a stale 1.
func (m *metrics) setDrift(drift map[string]string, known []string) {
	for _, service := range known {
		value := 0.0
		if _, bad := drift[service]; bad {
			value = 1
		}
		m.drift.WithLabelValues(service).Set(value)
	}
	for service := range drift {
		m.drift.WithLabelValues(service).Set(1)
	}
}

func isTerminalState(state string) bool {
	switch state {
	case deploy.StateHealthy, deploy.StateRolledBack, deploy.StateFailed:
		return true
	default:
		return false
	}
}
