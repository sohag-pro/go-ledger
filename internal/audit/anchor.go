package audit

// AnchorJob is the periodic background worker that records, and logs
// off-box, every tenant's current audit-chain head (Task 5.3, audit A2.4).
// See migration 0025's own doc comment for the full motivation: a hash chain
// alone proves only internal self-consistency, and a privileged actor with
// full database write access can rewrite a whole suffix of it consistently,
// leaving even a complete from-genesis Verify with nothing to find. An
// off-box anchor closes that gap: it is a copy of the head, taken at a point
// in time, that this database no longer controls once it has been logged
// and shipped elsewhere (Task 5.6). A later rewrite of that anchored row
// changes its row_hash; comparing the live head against the last anchor (or
// the shipped log line itself) reveals the rewrite.

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// DefaultAnchorInterval is how often an AnchorJob that was not given an
// explicit interval tries to become leader (if it is currently a follower)
// or records a fresh anchor for every tenant (if it already holds
// leadership). An hour is deliberately coarse: an anchor's value comes from
// existing off-box and outliving any single rewrite attempt, not from a tight
// recency bound the way the chainer's near-real-time draining needs.
const DefaultAnchorInterval = time.Hour

// anchorLockKey is the fixed Postgres session advisory-lock key every
// AnchorJob instance contends for, exactly like internal/audit.Chainer's own
// leaderLockKey and internal/webhook.Worker's, but a THIRD, distinct value:
// all three workers guard unrelated resources (the chainer's audit_log
// writes, the webhook worker's fan-out cursor, this job's audit_anchors
// rows), so none of them may ever contend on the same key, or an instance
// that wins one lock could wrongly believe itself the sole holder of
// another. It is an arbitrary int64 with no meaning beyond being fixed and
// distinct from 4_921_017 (the chainer) and 4_921_018 (the webhook worker);
// it must never change once chosen, for the same reason those two workers'
// own comments give.
const anchorLockKey int64 = 4_921_019

// AnchorJob is the leader-elected worker that, on Interval, for every tenant
// with at least one audit_log row, records that tenant's current chain head
// (chain_seq, row_hash) as a new audit_anchors row AND emits a structured
// slog line at info level carrying the same three values, so an off-box log
// shipper (Task 5.6) captures an external, independent record of it.
//
// Running more than one AnchorJob against the same database, one per app
// instance, is the intended deployment shape, exactly like Chainer and
// webhook.Worker: leader election (a session-level Postgres advisory lock,
// a DIFFERENT key than either of theirs) guarantees only one instance ever
// anchors on a given tick, so the same head is never logged and inserted
// twice by two racing instances in the same tick. Unlike Chainer, a lost
// race here is not a correctness hazard the way a forked audit chain would
// be: two anchors briefly disagreeing about which instance recorded a given
// tick's head would, at worst, produce one extra harmless audit_anchors row
// and one extra identical log line, never a wrong value. Leader election is
// still worth having anyway, purely to keep the off-box log stream free of
// that routine duplication.
type AnchorJob struct {
	pool       *pgxpool.Pool
	log        *slog.Logger
	interval   time.Duration
	signingKey []byte
}

// AnchorOption configures an AnchorJob.
type AnchorOption func(*AnchorJob)

// WithAnchorSigningKey makes the job sign every anchor it writes with key
// (domain.ComputeAnchorSignature), so audit_anchors is tamper-evident against a
// DB-privileged rewrite. A nil/empty key (the default) writes unsigned anchors,
// the prior behavior.
func WithAnchorSigningKey(key []byte) AnchorOption {
	return func(j *AnchorJob) { j.signingKey = key }
}

// NewAnchorJob returns an AnchorJob that reads and writes through pool.
// interval falls back to DefaultAnchorInterval when zero or negative; log
// falls back to slog.Default() when nil.
func NewAnchorJob(pool *pgxpool.Pool, log *slog.Logger, interval time.Duration, opts ...AnchorOption) *AnchorJob {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultAnchorInterval
	}
	j := &AnchorJob{pool: pool, log: log, interval: interval}
	for _, opt := range opts {
		opt(j)
	}
	return j
}

// Run is the job's long-running loop: until ctx is done, it repeatedly tries
// to become leader (pg_try_advisory_lock on a dedicated connection), and for
// as long as it holds leadership, anchors every tenant's head every
// interval. A follower (lock not acquired) waits interval and tries again
// without ever holding a connection while it waits. This is
// internal/audit.Chainer.Run's own shape, verbatim, against a different lock
// key.
func (j *AnchorJob) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		j.leadWhileHeld(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(j.interval):
		}
	}
}

// leadWhileHeld tries once to become leader; if it succeeds, it holds
// leadership (and the dedicated connection the advisory lock lives on),
// anchoring every tenant's head every j.interval, until ctx is done or a
// tick's query fails, then releases the lock and the connection. If it
// fails to become leader, it returns immediately: Run's own ticker paces the
// next attempt, so a follower never holds a connection just to wait. See
// internal/audit.Chainer.leadWhileHeld, whose structure this mirrors.
func (j *AnchorJob) leadWhileHeld(ctx context.Context) {
	conn, err := j.pool.Acquire(ctx)
	if err != nil {
		if ctx.Err() == nil {
			j.log.ErrorContext(ctx, "audit anchor job: acquire leader-election connection", "error", err)
		}
		return
	}

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", anchorLockKey).Scan(&acquired); err != nil {
		conn.Release()
		j.log.ErrorContext(ctx, "audit anchor job: try advisory lock", "error", err)
		return
	}
	if !acquired {
		conn.Release()
		return // another instance already holds it; Run's ticker retries later.
	}

	j.log.InfoContext(ctx, "audit anchor job: acquired leadership")
	defer func() {
		var unlocked bool
		_ = conn.QueryRow(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", anchorLockKey).Scan(&unlocked)
		conn.Release()
		j.log.InfoContext(ctx, "audit anchor job: released leadership")
	}()

	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()
	for {
		if n, err := j.anchorOnce(ctx, conn); err != nil {
			j.log.ErrorContext(ctx, "audit anchor job: tick failed, releasing leadership to re-contend", "error", err)
			return
		} else if n > 0 {
			j.log.InfoContext(ctx, "audit anchor job: tick complete", "tenants_anchored", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// AnchorOnce runs one anchoring pass directly against the pool, with no
// leader election: a convenience for tests and any caller that already knows
// it is the only writer, exactly like Chainer.DrainOnce and
// webhook.Worker.FanOutOnce. It returns how many tenants were anchored (and
// logged) in this pass.
func (j *AnchorJob) AnchorOnce(ctx context.Context) (int, error) {
	return j.anchorOnce(ctx, j.pool)
}

// anchorOnce reads every tenant's current head in one query (ListAuditHeads:
// avoids an N+1 GetAuditHead round trip per tenant), and for each, inserts an
// audit_anchors row and emits the off-box log line. It runs with the RLS GUC
// left unset (Task 5.4b): this is a cross-tenant background worker, never a
// per-tenant request-path read, exactly like the chainer's own drain queries
// and the webhook worker's fan-out.
//
// A failure inserting one tenant's anchor is logged and does not abort the
// rest of the pass: unlike the chainer's chain extension, where skipping a
// row would leave a later row's prev_hash pointing at the wrong predecessor,
// each tenant's anchor row is independent of every other tenant's, so one
// tenant's transient failure should not cost every other tenant this tick's
// anchor too. It still returns the first such error after attempting every
// tenant, so a caller (Run's leadWhileHeld) notices something is wrong
// without that one tenant blocking the rest.
func (j *AnchorJob) anchorOnce(ctx context.Context, db sqlc.DBTX) (int, error) {
	q := sqlc.New(db)
	heads, err := q.ListAuditHeads(ctx)
	if err != nil {
		return 0, err
	}

	var firstErr error
	anchored := 0
	for _, head := range heads {
		tenantID := head.TenantID.String()
		rowHash := head.RowHash.String
		var signature []byte
		if len(j.signingKey) > 0 {
			signature = domain.ComputeAnchorSignature(j.signingKey, tenantID, head.ChainSeq, rowHash)
		}
		if err := q.InsertAuditAnchor(ctx, sqlc.InsertAuditAnchorParams{
			TenantID:  head.TenantID,
			ChainSeq:  head.ChainSeq,
			RowHash:   rowHash,
			Signature: signature,
		}); err != nil {
			j.log.ErrorContext(ctx, "audit anchor job: insert anchor failed for tenant", "tenant_id", tenantID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// The off-box record (Task 5.6 ships this line elsewhere): tenant_id,
		// chain_seq, and row_hash are exactly what a later comparison against
		// the live head needs, and nothing else, so a log shipper capturing
		// only this line still has everything VerifyFromLatestAnchor's trust
		// model depends on.
		j.log.InfoContext(ctx, "audit anchor", "tenant_id", tenantID, "chain_seq", head.ChainSeq, "row_hash", rowHash)
		anchored++
	}
	return anchored, firstErr
}
