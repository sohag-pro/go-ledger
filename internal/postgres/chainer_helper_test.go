package postgres_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// drainMu serializes every drainChainer call in this test binary. Chainer's
// DrainOnce is documented as unsafe to call concurrently from more than one
// goroutine without Run's leader election around it (ADR-017: exactly one
// writer to audit_log at a time is the whole point). Production never
// violates that, since Run's advisory lock enforces it; this package's own
// tests would, since many call drainChainer from t.Parallel() subtests
// sharing one container. This mutex is what stands in for leader election
// here: it does not test leader election itself (see internal/audit's own
// tests for that), it just keeps this package's unrelated tests from
// tripping the single-writer invariant on each other.
var drainMu sync.Mutex

// discardTestLogger is a slog.Logger that throws its output away: the
// chainer logs at info/error level on every leadership change and failed
// drain, noise this package's tests do not want on stdout.
func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// drainChainer runs a Chainer against pool until tenant has no pending
// outbox rows left, or fails the test after a generous timeout.
//
// Since ADR-017, a post (or this package's own AppendAuditOutbox test calls)
// only writes an audit_outbox row; the tamper-evident audit_log chain is
// built by a separate background chainer. Tests that need to assert on the
// resulting audit_log rows (the chain, its hashes, tamper detection, and so
// on) call this right after writing the outbox row(s) they care about, so
// the rest of the test can go on asserting against audit_log exactly as it
// did when AppendAudit chained synchronously. The bounded retry loop (rather
// than a single drain pass) is deliberate: another parallel test in this
// package sharing the same container can transiently hold the xmin
// watermark back (ADR-017's ordering guarantee), so a single DrainOnce
// immediately after writing is not guaranteed to see a just-committed row
// yet, only guaranteed to see it eventually.
func drainChainer(t *testing.T, pool *pgxpool.Pool, tenant string) {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewRepository(pool)
	chainer := audit.NewChainer(pool, discardTestLogger(), time.Millisecond, 500)

	deadline := time.Now().Add(10 * time.Second)
	for {
		drainMu.Lock()
		_, err := chainer.DrainOnce(ctx)
		drainMu.Unlock()
		if err != nil {
			t.Fatalf("drain audit outbox: %v", err)
		}
		pending, err := repo.CountPendingOutbox(ctx, tenant)
		if err != nil {
			t.Fatalf("count pending outbox: %v", err)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("draining audit outbox for tenant %s timed out with %d rows still pending", tenant, pending)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
