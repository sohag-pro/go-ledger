package grpcserver_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// drainChainer runs a Chainer against pool until tenant has no pending
// outbox rows left, or fails the test after a generous timeout. Since
// ADR-017, PostTransaction/Convert only write an audit_outbox row; the
// tamper-evident audit_log chain is built by a separate background chainer.
// Tests that post through the real gRPC server and then want to read back
// audit_log rows (GetTransactionAudit, GetAccountAudit) call this first.
//
// This package's tests do not run t.Parallel() against the shared tenant
// (see server_test.go's testTenant), so a single Chainer instance here needs
// no additional coordination beyond the loop below.
func drainChainer(t *testing.T, pool *pgxpool.Pool, tenant string) {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewRepository(pool)
	chainer := audit.NewChainer(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, 500)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := chainer.DrainOnce(ctx); err != nil {
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
