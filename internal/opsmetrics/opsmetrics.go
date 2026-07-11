// Package opsmetrics runs the periodic background collector that keeps
// internal/metrics's operational gauges (audit outbox backlog and lag,
// webhook delivery backlog, and the balance-invariant canary) fresh from the
// database (Task 5.6a, audit A6.1).
//
// Unlike the audit chainer, the webhook worker, and the audit anchor job
// (internal/audit, internal/webhook), this collector needs no leader
// election. Every instance in a multi-instance deployment runs one
// unconditionally, and that is safe: each pass only reads cross-tenant
// aggregates and SETs a handful of Prometheus gauges, never writes to the
// database, so two instances collecting the same numbers at the same moment
// step on nothing. See cmd/server's own doc comment on the chainer/webhook
// wiring for why leader election exists there (a single, serialized writer)
// and why that reasoning does not apply here.
//
// Every query below runs with the RLS GUC app.tenant_id deliberately unset
// (this package never calls postgres.Repository's per-tenant withTenant, it
// queries the pool directly, exactly like internal/verify's restore-verifier
// does). Migrations 0024 and 0025's row-level security policies allow every
// row through when that GUC is unset or empty, precisely so operational
// tooling like this collector and the restore-verifier can read whole
// database aggregates without a tenant context; see those migrations' own
// comments for the policy text.
package opsmetrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/metrics"
)

// DefaultInterval is how often Collector.Run refreshes the gauges when
// METRICS_COLLECT_INTERVAL is unset.
const DefaultInterval = 30 * time.Second

// rowQuerier is the one pgxpool.Pool method the collector needs: a plain,
// unscoped QueryRow. Narrowing to this interface (rather than holding
// *pgxpool.Pool directly) lets tests substitute a fake that returns queued
// rows and errors without a real database, so the best-effort, continue-on-
// error behavior in collect can be proven fast and deterministically; the
// real DB-backed queries themselves are proven separately by this package's
// integration test.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Collector periodically refreshes internal/metrics's operational gauges
// from the database. The zero value is not usable; construct with
// NewCollector.
type Collector struct {
	db  rowQuerier
	log *slog.Logger
}

// NewCollector builds a Collector reading from pool.
func NewCollector(pool *pgxpool.Pool, log *slog.Logger) *Collector {
	return &Collector{db: pool, log: log}
}

// Run collects once immediately, then every interval, until ctx is
// cancelled. Mirrors cmd/server's runSeeder and runIdempotencySweep shape: a
// failed collection pass is logged (inside collect, per query) and never
// stops the loop, since a stale gauge value is a strictly better outcome
// than the loop giving up and the gauge going silent altogether.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	c.Collect(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Collect(ctx)
		}
	}
}

// Collect refreshes every gauge independently, once, right now: a query
// failure is logged and does not block the others, so one degraded table
// (say, a lock-contended webhook_deliveries) never blinds the
// balance-invariant canary too. Run calls this on a ticker; it is also
// exported so a caller (including this package's own integration test) can
// force a single collection pass without waiting for the next tick.
func (c *Collector) Collect(ctx context.Context) {
	c.collectOutboxBacklog(ctx)
	c.collectWebhookBacklog(ctx)
	c.collectBalanceInvariant(ctx)
}

// collectOutboxBacklog sets ledger_audit_outbox_pending and
// ledger_audit_outbox_lag_seconds: the count of unprocessed audit_outbox
// rows, and the age in seconds of the oldest one (0 when none are pending).
// This is the chain-backlog signal ADR-017 calls out: a chainer that has
// stopped advancing shows up here well before it shows up anywhere else.
func (c *Collector) collectOutboxBacklog(ctx context.Context) {
	var pending int64
	var lagSeconds float64
	err := c.db.QueryRow(ctx, `
		SELECT
			count(*),
			COALESCE(EXTRACT(EPOCH FROM (now() - min(created_at))), 0)
		FROM audit_outbox
		WHERE processed_at IS NULL
	`).Scan(&pending, &lagSeconds)
	if err != nil {
		c.log.ErrorContext(ctx, "opsmetrics: collect audit outbox backlog", "error", err)
		return
	}
	metrics.AuditOutboxPending.Set(float64(pending))
	metrics.AuditOutboxLagSeconds.Set(lagSeconds)
}

// collectWebhookBacklog sets ledger_webhook_deliveries_dead and
// ledger_webhook_deliveries_pending: rows that exhausted their retry budget,
// and rows still due for delivery or retry, respectively.
func (c *Collector) collectWebhookBacklog(ctx context.Context) {
	var dead, pending int64
	err := c.db.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'dead'),
			count(*) FILTER (WHERE status IN ('pending', 'failed'))
		FROM webhook_deliveries
	`).Scan(&dead, &pending)
	if err != nil {
		c.log.ErrorContext(ctx, "opsmetrics: collect webhook delivery backlog", "error", err)
		return
	}
	metrics.WebhookDeliveriesDead.Set(float64(dead))
	metrics.WebhookDeliveriesPending.Set(float64(pending))
}

// collectBalanceInvariant sets ledger_balance_invariant_violations: the
// balance-invariant canary. It runs the exact same query
// internal/verify's restore-verifier uses (per transaction, per currency,
// postings must sum to zero; see verify.go's own doc comment for why this
// mirrors the assert_txn_balanced trigger's rule), read-only, against the
// live database rather than a restored one. A healthy ledger always reports
// 0; any positive value means postings were recorded out of balance, a
// sev-1 signal (see deploy/alerts.yml's LedgerBalanceInvariantViolation).
func (c *Collector) collectBalanceInvariant(ctx context.Context) {
	var violations int64
	err := c.db.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT 1 FROM postings
			GROUP BY transaction_id, currency
			HAVING SUM(amount) <> 0
		) violations
	`).Scan(&violations)
	if err != nil {
		c.log.ErrorContext(ctx, "opsmetrics: collect balance invariant canary", "error", err)
		return
	}
	metrics.BalanceInvariantViolations.Set(float64(violations))
}
