package postgres

import (
	"context"
	"fmt"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// defaultMaxConns bounds pool size when the caller does not specify one. The
	// pool cap, not the number of goroutines, sets how many posting transactions
	// actually run at the database at once.
	defaultMaxConns = 10
	// Timeouts (milliseconds, as Postgres GUC strings) so a stuck statement,
	// lock wait, or idle-in-transaction session cannot hold a connection forever.
	statementTimeout = "5000"
	lockTimeout      = "3000"
	idleInTxTimeout  = "10000"
)

// NewPool opens a connection pool tuned for the ledger: a bounded number of
// connections and per-statement, per-lock, and idle-in-transaction timeouts.
// maxConns <= 0 uses defaultMaxConns. Callers (the server, tests) should use this
// rather than pgxpool.New so the safety limits are applied consistently.
func NewPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse pool config: %w", err)
	}
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = statementTimeout
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = lockTimeout
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = idleInTxTimeout

	// One span per SQL statement. otelpgx omits query arguments by default, so
	// account ids, amounts, and idempotency keys never leave the process as span
	// attributes (see ADR-010); we do not opt into WithIncludeQueryParameters.
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithTrimSQLInSpanName())

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: new pool: %w", err)
	}
	return pool, nil
}
