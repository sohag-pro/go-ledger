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

	// BuildInfo is always 1, labeled with the running binary's build
	// revision (git short SHA; "dev" outside a real build). Joining other
	// series against this one in a dashboard or alert shows which revision
	// produced them; see cmd/server, which sets the single "revision" series
	// once at startup.
	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "build_info",
		Help: "Always 1; labeled by the running binary's build revision.",
	}, []string{"revision"})

	// AuditOutboxPending is the count of audit_outbox rows not yet chained
	// (processed_at IS NULL): the chain backlog (ADR-017). Set by
	// internal/opsmetrics.Collector on an interval, not on the request path.
	AuditOutboxPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_audit_outbox_pending",
		Help: "Number of audit_outbox rows the chainer has not yet processed.",
	})

	// AuditOutboxLagSeconds is the age, in seconds, of the OLDEST unprocessed
	// audit_outbox row (0 when the outbox is empty): the real "is the
	// chainer keeping up" signal, since a small pending count can still mean
	// a stuck chainer if that count never drains.
	AuditOutboxLagSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_audit_outbox_lag_seconds",
		Help: "Age in seconds of the oldest unprocessed audit_outbox row; 0 when none are pending.",
	})

	// WebhookDeliveriesDead is the count of webhook_deliveries rows that
	// exhausted their retry budget (status = 'dead').
	WebhookDeliveriesDead = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_webhook_deliveries_dead",
		Help: "Number of webhook_deliveries rows in the dead state (retry budget exhausted).",
	})

	// WebhookDeliveriesPending is the count of webhook_deliveries rows still
	// due for delivery (status IN ('pending', 'failed')).
	WebhookDeliveriesPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_webhook_deliveries_pending",
		Help: "Number of webhook_deliveries rows still pending or awaiting retry.",
	})

	// BalanceInvariantViolations is the balance-invariant canary: the count
	// of (transaction_id, currency) posting groups whose postings do NOT sum
	// to zero. The core double-entry invariant (ADR-001, per currency since
	// ADR-014) guarantees this is 0 in a healthy ledger; any positive value
	// is a sev-1 signal, since it means money was recorded out of balance.
	// Mirrors the exact query internal/verify's restore-verifier uses.
	BalanceInvariantViolations = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_balance_invariant_violations",
		Help: "Count of transaction/currency posting groups whose postings do not sum to zero. Must be 0; a sev-1 signal otherwise.",
	})
)

func init() {
	registry.MustRegister(
		PostDuration, SerializationRetries, IdempotencyReplays, IdempotencyConflicts,
		BuildInfo,
		AuditOutboxPending, AuditOutboxLagSeconds,
		WebhookDeliveriesDead, WebhookDeliveriesPending,
		BalanceInvariantViolations,
	)
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
