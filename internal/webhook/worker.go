// Package webhook runs the single background worker that fans posted
// transactions out to tenant-configured webhook subscribers, signs each
// delivery, and retries it with exponential backoff until it succeeds or is
// given up on as dead (Task 4.1, audit A7.1).
//
// It is driven off audit_log (the chained, durable, chain_seq-ordered event
// stream ADR-017's chainer produces), not off audit_outbox: audit_log is the
// stable, already-ordered source of truth, so this worker never has to
// re-solve the ordering problem the chainer already solved. See
// docs/adr/017-multi-instance-audit-chain.md for that context and
// internal/audit.Chainer, whose leader-election and drain-loop shape this
// package deliberately mirrors.
package webhook

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// leaderLockKey is the fixed Postgres session advisory-lock key every Worker
// instance contends for, exactly like internal/audit.Chainer's own
// leaderLockKey, but a DIFFERENT fixed value: the two workers guard
// unrelated resources (audit_log's hash chain vs. webhook_deliveries'
// fan-out cursor), so they must never contend on the same key, or an
// instance that wins the chainer's lock and one that wins this one could
// each believe themselves the sole holder of the OTHER lock too. It is an
// arbitrary int64 with no meaning beyond being fixed and distinct from the
// chainer's 4_921_017 (and from any other advisory lock key this service
// ever takes); it must never change once chosen, for the same reason the
// chainer's own comment gives.
const leaderLockKey int64 = 4_921_018

// Default tuning: how often the leader runs one fan-out-then-delivery pass,
// how many rows each pass reads per scan, how many times a delivery is
// attempted before it is given up on as dead, and the exponential backoff
// schedule between failed attempts. See Config's own doc comment for why
// fan-out and delivery share one interval and one leader connection instead
// of running as two independently-paced loops.
const (
	DefaultInterval      = 2 * time.Second
	DefaultFanoutBatch   = 500
	DefaultDeliveryBatch = 50
	DefaultMaxAttempts   = 8
	DefaultBackoffBase   = 10 * time.Second
	DefaultBackoffCap    = 10 * time.Minute
	DefaultHTTPTimeout   = 10 * time.Second
)

// HTTP headers a delivery carries (Task 4.1). X-Ledger-Delivery-Id is the
// delivery row's own id: stable across every retry attempt of the same row,
// so a subscriber dedups repeat deliveries of the same event by it.
const (
	HeaderSignature  = "X-Ledger-Signature"
	HeaderDeliveryID = "X-Ledger-Delivery-Id"
	HeaderEvent      = "X-Ledger-Event"
)

// Config tunes a Worker; every zero field falls back to its Default constant
// in NewWorker, the same zero-falls-back-to-default style
// internal/audit.NewChainer uses.
//
// Fan-out and delivery run as two sequential steps of ONE loop, sharing one
// Interval and one dedicated leader connection, not as two independently
// scheduled loops or goroutines: a single pgx connection cannot safely be
// used by two goroutines at once, and running fan-out's cursor-advancing
// transaction and delivery's per-row attempts concurrently on the SAME
// connection would need exactly that. Doing fan-out then delivery in
// sequence, on the same connection, is what lets a lost leadership lock
// (the connection's very next query failing) be detected before either step
// does further work, mirroring ADR-017's CRITICAL 1 discipline for the part
// of this worker (fan-out's cursor advance) where a torn write would
// actually matter. Delivery's own DB reads and writes run against the
// general pool, not the leader connection, deliberately: an outbound HTTP
// call can take up to HTTPTimeout, and a database transaction (or a single
// dedicated connection) must never sit open for that long. This does mean a
// lost leadership lock is only noticed on the NEXT pass's fan-out step,
// a bounded window of at most one Interval; that is an acceptable staleness
// for a delivery subsystem whose whole design is already at-least-once and
// receiver-deduped (see Sign/SignatureHeader and HeaderDeliveryID), not a
// hazard on the order of ADR-017's chain-forking risk, which is why this
// worker does not need the chainer's absolute single-connection discipline
// for every query it runs.
type Config struct {
	Interval      time.Duration
	FanoutBatch   int
	DeliveryBatch int
	MaxAttempts   int
	BackoffBase   time.Duration
	BackoffCap    time.Duration
	HTTPTimeout   time.Duration
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.FanoutBatch <= 0 {
		c.FanoutBatch = DefaultFanoutBatch
	}
	if c.DeliveryBatch <= 0 {
		c.DeliveryBatch = DefaultDeliveryBatch
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = DefaultBackoffBase
	}
	if c.BackoffCap <= 0 {
		c.BackoffCap = DefaultBackoffCap
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = DefaultHTTPTimeout
	}
	return c
}

// dbtx is the minimal set of operations fan-out needs: sqlc's DBTX for plain
// reads, plus Begin for the cursor-advancing transaction. Both *pgxpool.Pool
// (fanOutOnce/deliverOnce, the no-leader-election convenience entry points
// tests call directly) and *pgxpool.Conn (the leader's own dedicated
// connection) satisfy it, exactly like internal/audit.Chainer's own dbtx.
type dbtx interface {
	sqlc.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Worker is the single background worker that fans audit_log events out to
// webhook_deliveries rows and delivers them, signed, with retry and backoff
// (Task 4.1). Running more than one Worker against the same database, one
// per app instance, is the intended deployment shape, exactly like
// internal/audit.Chainer: leader election guarantees only one of them is
// ever active, so webhook_deliveries has only one fan-out cursor writer and
// one delivery attempter at a time, regardless of how many instances run.
type Worker struct {
	pool   *pgxpool.Pool
	log    *slog.Logger
	cfg    Config
	client *http.Client
}

// NewWorker returns a Worker that reads and writes through pool, sending
// deliveries with an *http.Client timed out at cfg.HTTPTimeout. log falls
// back to slog.Default() when nil; every other zero Config field falls back
// to its Default constant.
func NewWorker(pool *pgxpool.Pool, log *slog.Logger, cfg Config) *Worker {
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.withDefaults()
	return &Worker{
		pool:   pool,
		log:    log,
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.HTTPTimeout},
	}
}

// Run is the worker's long-running loop: until ctx is done, it repeatedly
// tries to become leader (pg_try_advisory_lock on a dedicated connection),
// and for as long as it holds leadership, runs a fan-out-then-delivery pass
// every Interval. A follower (lock not acquired) waits Interval and tries
// again without ever holding a connection while it waits. This is
// internal/audit.Chainer.Run's own shape, verbatim, against a different
// lock key.
func (w *Worker) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		w.leadWhileHeld(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.cfg.Interval):
		}
	}
}

// leadWhileHeld tries once to become leader; if it succeeds, it holds
// leadership (and the dedicated connection the advisory lock lives on),
// running a fan-out pass (on that connection) followed by a delivery pass
// (on the general pool) every Interval, until ctx is done or the fan-out
// step's connection reports the lock session is gone. See
// internal/audit.Chainer.leadWhileHeld, whose structure this mirrors, and
// Config's own doc comment for why only fan-out, not delivery, runs on the
// dedicated connection.
func (w *Worker) leadWhileHeld(ctx context.Context) {
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			w.log.ErrorContext(ctx, "webhook worker: acquire leader-election connection", "error", err)
		}
		return
	}

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", leaderLockKey).Scan(&acquired); err != nil {
		conn.Release()
		w.log.ErrorContext(ctx, "webhook worker: try advisory lock", "error", err)
		return
	}
	if !acquired {
		conn.Release()
		return // another instance already holds it; Run's ticker retries later.
	}

	w.log.InfoContext(ctx, "webhook worker: acquired leadership")
	defer func() {
		var unlocked bool
		_ = conn.QueryRow(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", leaderLockKey).Scan(&unlocked)
		conn.Release()
		w.log.InfoContext(ctx, "webhook worker: released leadership")
	}()

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		if _, err := w.fanOutOnce(ctx, conn); err != nil {
			w.log.ErrorContext(ctx, "webhook worker: fan-out failed, releasing leadership to re-contend", "error", err)
			return
		}
		if delivered, retried, dead, err := w.deliverBatchLoop(ctx, w.pool); err != nil {
			w.log.ErrorContext(ctx, "webhook worker: delivery pass failed", "error", err)
		} else if delivered+retried+dead > 0 {
			w.log.InfoContext(ctx, "webhook worker: delivery pass", "delivered", delivered, "retried", retried, "dead", dead)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// FanOutOnce runs one full fan-out pass directly against the pool, with no
// leader election: a convenience for tests and any caller that already
// knows it is the only writer, exactly like Chainer.DrainOnce. It returns
// how many webhook_deliveries rows it created (across every subscription
// and every audit_log row scanned), not how many audit_log rows it read.
func (w *Worker) FanOutOnce(ctx context.Context) (int, error) {
	return w.fanOutOnce(ctx, w.pool)
}

// DeliverOnce runs one delivery pass directly against the pool, with no
// leader election: the same convenience FanOutOnce is for the fan-out side.
// It returns how many deliveries succeeded, were retried, and were given up
// on as dead in this pass.
func (w *Worker) DeliverOnce(ctx context.Context) (delivered, retried, dead int, err error) {
	return w.deliverBatch(ctx, w.pool)
}

// fanOutOnce repeatedly runs one fan-out batch until a batch reads no more
// audit_log rows, then returns the total webhook_deliveries rows created.
func (w *Worker) fanOutOnce(ctx context.Context, db dbtx) (int, error) {
	total := 0
	for {
		n, err := w.fanOutBatch(ctx, db)
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

// deliverBatchLoop repeatedly runs one delivery batch until a batch finds no
// more due rows, accumulating totals across every batch in this pass. A
// single batch does not necessarily drain every currently-due row (there may
// be more than DeliveryBatch), so this loops the same way fanOutOnce does
// for fan-out batches.
func (w *Worker) deliverBatchLoop(ctx context.Context, db dbtx) (delivered, retried, dead int, err error) {
	for {
		d, r, x, err := w.deliverBatch(ctx, db)
		delivered += d
		retried += r
		dead += x
		if err != nil {
			return delivered, retried, dead, err
		}
		if d+r+x == 0 {
			return delivered, retried, dead, nil
		}
	}
}

// backoffFor returns how long to wait before retrying a delivery that has
// just failed for the attempt'th time (1-based: attempt is the NEW attempts
// count after this failure is recorded). It is exponential (base, then
// x3 each further attempt) and capped, e.g. with the defaults: 10s, 30s,
// 90s, 270s, then capped at 10m from the fifth attempt on. Unlike
// postgres.retryBackoff's jittered backoff (built to spread a crowd of
// concurrently conflicting transactions apart), this worker never has more
// than one leader attempting a given delivery at a time, so there is no
// thundering herd to jitter away: a deterministic schedule is simpler to
// reason about and to test.
func backoffFor(attempt int, base, backoffCap time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 3
		if d > backoffCap {
			return backoffCap
		}
	}
	if d > backoffCap {
		return backoffCap
	}
	return d
}
