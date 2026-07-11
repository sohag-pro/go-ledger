package ledger_test

import (
	"context"
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
// tests would, since many post through TransactionService/Post from
// t.Parallel() subtests sharing one container. This mutex stands in for
// leader election here; see internal/audit's own tests for leader election
// itself.
var drainMu sync.Mutex

// drainChainer runs a Chainer against pool until tenant has no pending
// outbox rows left, or fails the test after a generous timeout.
//
// Since ADR-017, TransactionService.Post and Convert only write an
// audit_outbox row inside their posting transaction; the tamper-evident
// audit_log chain is built by a separate background chainer. Tests that
// posted through the real service and then want to assert on the resulting
// audit_log rows (via AuditService.Verify, ListAuditByTransaction,
// ListAuditByAccount, or a raw repo read) call this first, so the rest of
// the test can go on asserting against audit_log exactly as it did when the
// chain was extended synchronously inside Post/Convert.
func drainChainer(t *testing.T, pool *pgxpool.Pool, tenant string) {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewRepository(pool)
	chainer := audit.NewChainer(pool, discardLogger(), time.Millisecond, 500)

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
