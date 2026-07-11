package opsmetrics

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sohag-pro/go-ledger/internal/metrics"
)

// fakeRow is a canned pgx.Row: either an error, or a fixed set of column
// values Scan copies into the destinations collect passed, in order.
type fakeRow struct {
	err  error
	vals []any
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch v := d.(type) {
		case *int64:
			*v = r.vals[i].(int64)
		case *float64:
			*v = r.vals[i].(float64)
		default:
			return errors.New("fakeRow: unsupported scan destination")
		}
	}
	return nil
}

// fakePool is a rowQuerier that dispatches on a substring of the query text
// (audit_outbox / webhook_deliveries / postings), so it works regardless of
// the order collect issues the three queries in. Each query's response is
// swappable per test, and every dispatched call is signaled on calls so a
// test can wait for a specific number of collection passes without a sleep.
type fakePool struct {
	mu sync.Mutex

	outbox  fakeRow
	webhook fakeRow
	balance fakeRow

	calls chan string
}

func newFakePool() *fakePool {
	return &fakePool{
		outbox:  fakeRow{vals: []any{int64(0), float64(0)}},
		webhook: fakeRow{vals: []any{int64(0), int64(0)}},
		balance: fakeRow{vals: []any{int64(0)}},
		calls:   make(chan string, 64),
	}
}

func (p *fakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case strings.Contains(sql, "audit_outbox"):
		p.calls <- "outbox"
		return p.outbox
	case strings.Contains(sql, "webhook_deliveries"):
		p.calls <- "webhook"
		return p.webhook
	case strings.Contains(sql, "postings"):
		p.calls <- "balance"
		return p.balance
	default:
		p.calls <- "unexpected"
		return fakeRow{err: errors.New("fakePool: unexpected query: " + sql)}
	}
}

// waitCalls drains n signals from calls, failing the test if they do not
// arrive within a generous timeout.
func waitCalls(t *testing.T, calls chan string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for collector call %d/%d", i+1, n)
		}
	}
}

// TestCollect_SetsGaugesFromQueryResults proves collect scans every query's
// result into the right gauge: this is a fast, DB-free check of the wiring
// (column order, gauge assignment) that opsmetrics_integration_test.go then
// proves end to end against a real database.
func TestCollect_SetsGaugesFromQueryResults(t *testing.T) {
	pool := newFakePool()
	pool.outbox = fakeRow{vals: []any{int64(7), float64(123.5)}}
	pool.webhook = fakeRow{vals: []any{int64(3), int64(9)}}
	pool.balance = fakeRow{vals: []any{int64(2)}}

	c := &Collector{db: pool, log: slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))}
	c.Collect(context.Background())

	if got := testutil.ToFloat64(metrics.AuditOutboxPending); got != 7 {
		t.Errorf("AuditOutboxPending = %v, want 7", got)
	}
	if got := testutil.ToFloat64(metrics.AuditOutboxLagSeconds); got != 123.5 {
		t.Errorf("AuditOutboxLagSeconds = %v, want 123.5", got)
	}
	if got := testutil.ToFloat64(metrics.WebhookDeliveriesDead); got != 3 {
		t.Errorf("WebhookDeliveriesDead = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.WebhookDeliveriesPending); got != 9 {
		t.Errorf("WebhookDeliveriesPending = %v, want 9", got)
	}
	if got := testutil.ToFloat64(metrics.BalanceInvariantViolations); got != 2 {
		t.Errorf("BalanceInvariantViolations = %v, want 2", got)
	}
}

// TestCollect_BestEffort_OneQueryFailingDoesNotBlockOthers proves the
// best-effort contract (Task 5.6a brief): a failing query is logged, and the
// OTHER gauges still update in the same pass, rather than the whole
// collection aborting on the first error.
func TestCollect_BestEffort_OneQueryFailingDoesNotBlockOthers(t *testing.T) {
	pool := newFakePool()
	pool.webhook = fakeRow{err: errors.New("connection reset")}
	pool.outbox = fakeRow{vals: []any{int64(4), float64(60)}}
	pool.balance = fakeRow{vals: []any{int64(0)}}

	var logBuf bytes.Buffer
	c := &Collector{db: pool, log: slog.New(slog.NewTextHandler(&logBuf, nil))}
	c.Collect(context.Background())

	if got := testutil.ToFloat64(metrics.AuditOutboxPending); got != 4 {
		t.Errorf("AuditOutboxPending = %v, want 4 (should still update despite webhook query failing)", got)
	}
	if got := testutil.ToFloat64(metrics.BalanceInvariantViolations); got != 0 {
		t.Errorf("BalanceInvariantViolations = %v, want 0", got)
	}
	if !strings.Contains(logBuf.String(), "collect webhook delivery backlog") {
		t.Errorf("log missing the failed webhook query line: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "connection reset") {
		t.Errorf("log missing the underlying error: %q", logBuf.String())
	}
}

// TestCollector_Run_CollectsImmediatelyThenOnIntervalThenStops proves Run
// mirrors cmd/server's runSeeder/runIdempotencySweep shape: one collection
// pass immediately (not waiting a full interval first), more on every tick,
// and a clean return once ctx is cancelled.
func TestCollector_Run_CollectsImmediatelyThenOnIntervalThenStops(t *testing.T) {
	pool := newFakePool()
	c := &Collector{db: pool, log: slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx, time.Millisecond)
		close(done)
	}()

	// Each pass issues 3 queries (outbox, webhook, balance); wait for two
	// full passes (the immediate one plus at least one tick) = 6 calls.
	waitCalls(t, pool.calls, 6)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
