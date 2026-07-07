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
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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

	// IdempotencyReplays counts posts short-circuited by a matching
	// Idempotency-Key: the original transaction was returned, no new one written.
	IdempotencyReplays = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "transaction_idempotency_replays_total",
		Help: "Number of transaction posts served as an idempotent replay.",
	})

	// IdempotencyConflicts counts posts rejected because an Idempotency-Key was
	// reused with a different request body.
	IdempotencyConflicts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "transaction_idempotency_conflicts_total",
		Help: "Number of transaction posts rejected for an idempotency-key/body mismatch.",
	})
)

func init() {
	registry.MustRegister(PostDuration, SerializationRetries, IdempotencyReplays, IdempotencyConflicts)
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

// MeterProvider builds an OpenTelemetry MeterProvider whose Prometheus exporter
// writes into this package's registry, so the RED metrics emitted by otelhttp and
// otelgrpc are scraped from the same /metrics endpoint as the native collectors.
// Scope and target info are dropped to keep the output uncluttered; runtime
// instrumentation is deliberately not enabled here to avoid duplicating the native
// go_* and process_* collectors.
func MeterProvider() (*sdkmetric.MeterProvider, error) {
	exporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		otelprom.WithoutScopeInfo(),
		otelprom.WithoutTargetInfo(),
	)
	if err != nil {
		return nil, err
	}
	return sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter)), nil
}
