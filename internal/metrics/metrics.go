// Package metrics holds the Prometheus collectors for the ledger and exposes an
// HTTP handler to scrape them. Collectors are package-level vars registered once
// with a dedicated registry, so importing the package is enough to record into
// them; Handler() serves that registry.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var registry = prometheus.NewRegistry()

var (
	// PostDuration measures end-to-end transaction posting latency, labeled by
	// outcome ("committed" or "failed"). Buckets are tuned for the millisecond
	// range we expect locally (see the Week 4 latency baselines).
	PostDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "transaction_post_duration_seconds",
		Help:    "Time to post a transaction, from service entry to commit, by outcome.",
		Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	}, []string{"outcome"})

	// SerializationRetries counts how often a posting transaction was retried
	// after a SERIALIZABLE conflict. A climbing rate signals write contention on
	// hot accounts.
	SerializationRetries = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "transaction_post_serialization_retries_total",
		Help: "Number of posting transactions retried after a serialization conflict.",
	})
)

func init() {
	registry.MustRegister(PostDuration, SerializationRetries)
	// Standard runtime and process metrics (go_*, process_*) for baseline
	// observability: goroutines, memory, GC, open FDs, CPU.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// Handler serves the ledger's metrics in the Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
