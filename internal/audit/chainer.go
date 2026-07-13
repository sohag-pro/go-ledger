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
	"github.com/jackc/pgx/v5/pgconn"
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

// outboxUniqueConstraint is the UNIQUE constraint audit_log.outbox_id carries
// (migration 0016, ADR-017 MINOR 3): defense in depth against a double-chain
// of the same outbox row. It should never fire once the CRITICAL 1 fix below
// (every drain query, including the per-row insert, runs on the SAME
// connection that holds the leader-election lock) is in place: a lost lock
// session then aborts the in-flight drain instead of letting it silently
// continue on a healthy connection elsewhere in the pool. It stays as a
// backstop for anything that reasoning did not anticipate (a legitimate
// retry racing another writer that briefly held the lock, a future code path
// that reintroduces a second connection), so a double-chain becomes a failed
// insert this chainer handles gracefully, not a silent fork.
const outboxUniqueConstraint = "audit_log_outbox_id_key"

// dbtx is the minimal set of operations drainBatch and chainOne need: sqlc's
// DBTX for plain reads, plus Begin for the per-row insert-and-mark
// transaction. Both *pgxpool.Pool (DrainOnce, called directly by tests and by
// nothing else: no leader election is in the picture, so there is no lock
// connection to pin drain work to) and *pgxpool.Conn (leadWhileHeld, where
// pinning every drain query to the single connection that holds the advisory
// lock is the whole point, ADR-017 CRITICAL 1) satisfy it.
type dbtx interface {
	sqlc.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

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
// holding a connection while it waits.
//
// If the leader loses its lock session (the leader process crashes, the
// connection drops, or an operator/Postgres itself terminates that backend,
// for example during a restart or failover), leadWhileHeld notices on its
// very next drain query (ADR-017 CRITICAL 1: every drain query runs on that
// same session) and returns, releasing leadership; Run's own loop then waits
// interval and re-contends, exactly like a follower that never held the lock
// at all. The next instance to acquire the lock resumes exactly where the
// outbox's own processed_at state leaves off, with no fork: at most one
// instance is ever mid-drain at a time.
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
// draining the outbox every c.interval, until ctx is done or a drain query
// fails, then releases the lock and the connection. If it fails to become
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
		// for a dropped connection to be noticed. If the connection is
		// already broken (the very case leadWhileHeld is about to have
		// returned for), this call simply fails silently: there is no
		// session left to unlock.
		var unlocked bool
		_ = conn.QueryRow(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", leaderLockKey).Scan(&unlocked)
		conn.Release()
		c.log.InfoContext(ctx, "audit chainer: released leadership")
	}()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		// Every drain query for this leadership term runs on conn, the SAME
		// connection the advisory lock lives on (ADR-017 CRITICAL 1). This is
		// the fix: previously drain work ran on c.pool (any connection), so a
		// lock session lost to a Postgres restart, failover, or
		// pg_terminate_backend went unnoticed here, this instance kept
		// draining on a perfectly healthy other connection, and whichever
		// instance next acquired the now-free lock drained concurrently with
		// it, both reading the same chain head and forking it. Now, the
		// moment the lock session is gone, conn itself is the thing that is
		// gone: the very next query on it fails, and that failure is treated
		// as leadership lost.
		if _, err := c.drainOnce(ctx, conn); err != nil {
			c.log.ErrorContext(ctx, "audit chainer: drain failed, releasing leadership to re-contend", "error", err)
			return
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
// DrainOnce does not participate in leader election and runs its queries
// against c.pool (any connection), not any single dedicated one. It is
// exported as a convenience for a caller that already knows it is the only
// writer: tests call it directly against a single Chainer with no other
// writer in the picture. Calling it from more than one goroutine or process
// concurrently, without Run's leader election around it, is not safe: it
// would defeat the "exactly one writer" property audit_log depends on
// (ADR-017). Run itself never calls DrainOnce: leadWhileHeld calls the
// unexported drainOnce directly, on its own lock-holding connection, so its
// drain work has the CRITICAL 1 guarantee DrainOnce's pool-wide queries do
// not need, since it is never running concurrently with another writer.
func (c *Chainer) DrainOnce(ctx context.Context) (int, error) {
	return c.drainOnce(ctx, c.pool)
}

// drainOnce is DrainOnce's implementation, parameterized on the connection
// (or pool) every query in this pass runs against.
func (c *Chainer) drainOnce(ctx context.Context, db dbtx) (int, error) {
	total := 0
	for {
		n, err := c.drainBatch(ctx, db)
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
func (c *Chainer) drainBatch(ctx context.Context, db dbtx) (int, error) {
	q := sqlc.New(db)

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
		if err := c.chainOne(ctx, db, row, lastHash); err != nil {
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
// writer. Neither chain_seq (left to its column default) nor outbox_id is
// ever hashed (ADR-017 IMPORTANT 2 / MINOR 3, migration 0016): both are
// purely structural, so every already-stored row_hash stays reproducible.
//
// If the insert hits audit_log's outbox_id UNIQUE constraint (MINOR 3's
// backstop), this outbox row was already chained by some other writer: the
// attempted insert is rolled back, the outbox row is marked processed on its
// own (idempotent) statement so it is not retried forever, and this
// tenant's cached hash is evicted so the next row for it re-reads the true
// head from GetLastAuditHash instead of extending from a hash that was never
// actually inserted. That is a graceful no-op, not an error: the row is, in
// fact, chained, just not by this call.
func (c *Chainer) chainOne(ctx context.Context, db dbtx, row sqlc.ScanUnprocessedAuditOutboxRow, lastHash map[string]string) error {
	tenantID := row.TenantID.String()
	prev, ok := lastHash[tenantID]
	if !ok {
		last, err := sqlc.New(db).GetLastAuditHash(ctx, row.TenantID)
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
		TransactionID: pgUUIDToString(row.TransactionID),
		Actor:         row.Actor,
		Before:        row.Before,
		After:         row.After,
		CreatedAt:     row.OccurredAt.UTC().Truncate(time.Microsecond),
		PrevHash:      prev,
		SubjectType:   row.SubjectType.String,
		SubjectID:     pgUUIDToString(row.SubjectID),
		HashVersion:   int(row.HashVersion),
	}
	entry.RowHash = domain.ComputeAuditRowHash(tenantID, entry, prev)

	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	txq := sqlc.New(tx)
	insertErr := txq.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
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
		OutboxID:      pgtype.Int8{Int64: row.ID, Valid: true},
		SubjectType:   row.SubjectType,
		SubjectID:     row.SubjectID,
		HashVersion:   row.HashVersion,
	})
	if insertErr != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		if isDuplicateOutboxChain(insertErr) {
			if markErr := sqlc.New(db).MarkAuditOutboxProcessed(ctx, row.ID); markErr != nil {
				return fmt.Errorf("mark outbox processed after duplicate-chain backstop: %w", markErr)
			}
			delete(lastHash, tenantID)
			return nil
		}
		return fmt.Errorf("insert audit log: %w", insertErr)
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

// pgUUIDToString maps a nullable pgtype.UUID column back onto the plain,
// possibly-empty string domain.AuditEntry uses (ADR-025, migration 0034):
// "" when the column is NULL (a non-transaction lifecycle event's
// transaction_id, or any row's subject_id before ADR-025), otherwise its
// string form. pgtype.UUID.String() does NOT check Valid itself (it happily
// formats the zero value), so calling it directly on a possibly-null column
// would silently turn NULL into the nil UUID string instead of "": every
// read of a nullable audit uuid column in this package must go through this
// helper, never row.Field.String() directly.
func pgUUIDToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// isDuplicateOutboxChain reports whether err is audit_log's outbox_id unique
// violation (migration 0016, ADR-017 MINOR 3): a second attempt, by any
// writer, to chain the same audit_outbox row that some writer already
// chained. It is deliberately scoped to that one constraint, not any unique
// violation (for example a UUIDv7 id collision, astronomically unlikely but
// a different failure entirely), so an unrelated conflict is never silently
// swallowed here.
func isDuplicateOutboxChain(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" && pgErr.ConstraintName == outboxUniqueConstraint
	}
	return false
}
