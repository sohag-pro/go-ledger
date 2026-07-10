// Package audit runs the single background chainer that builds each
// tenant's tamper-evident audit hash chain from the audit_outbox
// transactional outbox (ADR-017). See
// docs/adr/017-multi-instance-audit-chain.md for the full design and
// rationale; this file is that decision made real.
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// DefaultInterval is how often a Chainer that was not given an explicit
// interval tries to become leader (if it is currently a follower) or drains
// the outbox again (if it already holds leadership).
const DefaultInterval = time.Second

// DefaultBatch is how many outbox rows one drain pass reads per scan when
// the caller did not set an explicit batch size.
const DefaultBatch = 500

// leaderLockKey is the fixed Postgres session advisory-lock key every
// Chainer instance contends for (ADR-017, "Exactly one chainer: leader
// election via advisory lock"): whichever instance holds it is the sole
// writer to audit_log, and every other instance idles and retries. It is an
// arbitrary int64 with no meaning beyond being fixed and distinct from any
// other advisory lock key this service might ever take out (there are none
// today); it must never change once chosen, since two binaries contending on
// different keys during a rollout would both believe themselves the sole
// leader.
const leaderLockKey int64 = 4_921_017 // arbitrary, fixed for the life of this service

// Chainer is the single background worker that drains audit_outbox in
// transaction-commit order and builds each tenant's tamper-evident audit
// hash chain (ADR-017): the same audit_log schema and the same
// domain.ComputeAuditRowHash hashing the old, now-removed synchronous
// AppendAudit path used, so existing verify logic and any already-chained
// rows stay valid.
//
// Running more than one Chainer against the same database, one per app
// instance, is the intended deployment shape, not a hazard to avoid: leader
// election (a session-level Postgres advisory lock) guarantees only one of
// them ever drains at a time, so audit_log only ever has one writer
// regardless of how many instances run.
type Chainer struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	interval time.Duration
	batch    int
}

// NewChainer returns a Chainer that reads and writes through pool. interval
// and batch fall back to DefaultInterval / DefaultBatch when zero or
// negative; log falls back to slog.Default() when nil.
func NewChainer(pool *pgxpool.Pool, log *slog.Logger, interval time.Duration, batch int) *Chainer {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	if batch <= 0 {
		batch = DefaultBatch
	}
	return &Chainer{pool: pool, log: log, interval: interval, batch: batch}
}

// Run is the chainer's long-running loop: until ctx is done, it repeatedly
// tries to become leader (pg_try_advisory_lock on a dedicated connection),
// and for as long as it holds leadership, drains the outbox every interval.
// A follower (lock not acquired) waits interval and tries again without ever
// holding a connection while it waits. If the current leader crashes or its
// connection drops, Postgres releases the session-level advisory lock
// automatically, and the next instance to try acquires it and takes over
// with no fork: the outbox's own processed_at state is exactly where the new
// leader resumes, nothing more.
//
// Run returns only when ctx is done; callers run it in its own goroutine and
// cancel ctx to stop it (see cmd/server's wiring).
func (c *Chainer) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		c.leadWhileHeld(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.interval):
		}
	}
}

// leadWhileHeld tries once to become leader; if it succeeds, it holds
// leadership (and the dedicated connection the advisory lock lives on),
// draining the outbox every c.interval, until ctx is done or the connection
// is lost, then releases the lock and the connection. If it fails to become
// leader, it returns immediately: Run's own ticker paces the next attempt,
// so a follower never holds a connection just to wait.
func (c *Chainer) leadWhileHeld(ctx context.Context) {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			c.log.ErrorContext(ctx, "audit chainer: acquire leader-election connection", "error", err)
		}
		return
	}

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", leaderLockKey).Scan(&acquired); err != nil {
		conn.Release()
		c.log.ErrorContext(ctx, "audit chainer: try advisory lock", "error", err)
		return
	}
	if !acquired {
		conn.Release()
		return // another instance already holds it; Run's ticker retries later.
	}

	c.log.InfoContext(ctx, "audit chainer: acquired leadership")
	defer func() {
		// Best-effort, on a detached context: ctx may already be cancelled
		// (shutdown), but the unlock should still run so this session does
		// not hold the lock a moment longer than it has to. Postgres would
		// release it anyway once the connection closes, but releasing it
		// explicitly here means the next leader does not even have to wait
		// for a dropped connection to be noticed.
		var unlocked bool
		_ = conn.QueryRow(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", leaderLockKey).Scan(&unlocked)
		conn.Release()
		c.log.InfoContext(ctx, "audit chainer: released leadership")
	}()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		if _, err := c.DrainOnce(ctx); err != nil {
			c.log.ErrorContext(ctx, "audit chainer: drain failed", "error", err)
			// A broken connection surfaces here as a query error on c.pool,
			// not necessarily on conn (drain work uses its own connections);
			// keep leading and let the next tick retry, exactly as a
			// transient error on any other iteration would.
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DrainOnce runs one full drain pass: it repeatedly scans and chains
// eligible outbox rows (c.batch at a time, oldest commit order first) until
// a scan comes back empty, then returns how many rows it chained.
//
// DrainOnce does not participate in leader election. It is exported as a
// convenience for a caller that already knows it is the only writer: Run's
// own loop calls it while holding leadership, and tests call it directly
// against a single Chainer with no other writer in the picture. Calling it
// from more than one goroutine or process concurrently, without Run's leader
// election around it, is not safe: it would defeat the "exactly one writer"
// property audit_log depends on (ADR-017).
func (c *Chainer) DrainOnce(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := c.drainBatch(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

// drainBatch reads the current watermark, scans up to c.batch unprocessed
// outbox rows below it, and chains each one in commit order, returning how
// many it processed.
func (c *Chainer) drainBatch(ctx context.Context) (int, error) {
	q := sqlc.New(c.pool)

	xmin, err := q.AuditOutboxWatermark(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit chainer: watermark: %w", err)
	}

	rows, err := q.ScanUnprocessedAuditOutbox(ctx, sqlc.ScanUnprocessedAuditOutboxParams{
		Xmin:       xmin,
		BatchLimit: int32(c.batch), //nolint:gosec // c.batch is an application-configured, small positive value
	})
	if err != nil {
		return 0, fmt.Errorf("audit chainer: scan outbox: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	// Seeded from the database the first time a tenant appears in this
	// batch, then kept current in memory as rows for that tenant are
	// chained, so multiple events for one tenant within this batch chain
	// sequentially without a redundant read per row (ADR-017's drainAll).
	lastHash := make(map[string]string)
	for _, row := range rows {
		if err := c.chainOne(ctx, row, lastHash); err != nil {
			return 0, fmt.Errorf("audit chainer: chain outbox row %d: %w", row.ID, err)
		}
	}
	return len(rows), nil
}

// chainOne chains a single outbox row: it resolves the tenant's current
// chain tail (from lastHash if this tenant already appeared earlier in this
// batch, otherwise from GetLastAuditHash), computes the next link exactly as
// the old AppendAudit did, and inserts the audit_log row and marks the
// outbox row processed in one transaction, so a crash between the two is
// impossible: either both happen, or neither does, and a retried drain finds
// the row still unprocessed with no half-written audit_log row ahead of it.
//
// row.OccurredAt, read back exactly as audit_outbox stored it, becomes the
// chained row's CreatedAt: it is what gets hashed and what audit_log stores,
// with only the microsecond truncation every stored timestamptz already
// implies (see migration 0015 and the old AppendAudit's own doc comment for
// why that truncation is what keeps a stored row_hash recomputable). This is
// what makes the chainer produce byte-identical row_hash values to the old
// synchronous path: same fields, same order, same hash function
// (domain.ComputeAuditRowHash), just computed later and by a different
// writer.
func (c *Chainer) chainOne(ctx context.Context, row sqlc.ScanUnprocessedAuditOutboxRow, lastHash map[string]string) error {
	tenantID := row.TenantID.String()
	prev, ok := lastHash[tenantID]
	if !ok {
		last, err := sqlc.New(c.pool).GetLastAuditHash(ctx, row.TenantID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			prev = domain.AuditGenesisHash
		case err != nil:
			return fmt.Errorf("get last audit hash: %w", err)
		case last.Valid:
			prev = last.String
		default:
			// A pre-migration legacy row with a NULL row_hash (see the old
			// AppendAudit's own comment): treat it as an unchained genesis,
			// the same choice the old path made.
			prev = domain.AuditGenesisHash
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate audit id: %w", err)
	}

	entry := domain.AuditEntry{
		ID:            id.String(),
		Action:        row.Action,
		TransactionID: row.TransactionID.String(),
		Actor:         row.Actor,
		Before:        row.Before,
		After:         row.After,
		CreatedAt:     row.OccurredAt.UTC().Truncate(time.Microsecond),
		PrevHash:      prev,
	}
	entry.RowHash = domain.ComputeAuditRowHash(tenantID, entry, prev)

	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	txq := sqlc.New(tx)
	if err := txq.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		ID:            id,
		TenantID:      row.TenantID,
		Action:        entry.Action,
		TransactionID: row.TransactionID,
		Actor:         entry.Actor,
		Before:        entry.Before,
		After:         entry.After,
		CreatedAt:     entry.CreatedAt,
		PrevHash:      pgtype.Text{String: entry.PrevHash, Valid: true},
		RowHash:       pgtype.Text{String: entry.RowHash, Valid: true},
	}); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return fmt.Errorf("insert audit log: %w", err)
	}
	if err := txq.MarkAuditOutboxProcessed(ctx, row.ID); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return fmt.Errorf("mark outbox processed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	lastHash[tenantID] = entry.RowHash
	return nil
}
