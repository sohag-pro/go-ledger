// Package postgres is the Postgres storage adapter for the ledger. It implements
// the domain.Repository port on top of pgx and sqlc-generated queries. It holds
// no business rules: the double-entry invariant is enforced by the domain
// (Transaction.Validate) and, from Week 4, by a database CHECK constraint.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/metrics"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

const (
	// maxPostAttempts bounds how many times RunInTx replays fn after a
	// serialization conflict before giving up.
	maxPostAttempts = 25
	// backoffBase and backoffCap bound the exponential backoff between retries.
	backoffBase = time.Millisecond
	backoffCap  = 100 * time.Millisecond
)

// retryBackoff returns how long to wait before the given retry attempt (1-based).
// It is exponential, capped, with full jitter: the random spread is what stops a
// crowd of conflicting transactions from retrying in lockstep and colliding
// again. See "Exponential Backoff And Jitter" (AWS Architecture Blog).
func retryBackoff(attempt int) time.Duration {
	exp := backoffBase << uint(attempt-1)
	if exp <= 0 || exp > backoffCap { // overflow or past the cap
		exp = backoffCap
	}
	return time.Duration(rand.Int64N(int64(exp) + 1)) //nolint:gosec // jitter, not crypto
}

// keyedMutex is a set of independent mutexes, one per key, created lazily on
// first use. It serializes callers that share a key while leaving callers
// with different keys fully concurrent, without ever touching a database
// connection: waiters block on ordinary Go scheduling, not on anything held
// against Postgres.
//
// Each key's mutex is a capacity-1 channel rather than a sync.Mutex so that a
// waiter can give up: acquiring it is a select between sending on the channel
// and the caller's context being done, so a cancelled or timed-out caller
// stops waiting immediately instead of blocking until the current holder
// releases.
//
// The underlying sync.Map grows by one entry per distinct key ever seen and
// is never evicted. That is deliberate: keys here are tenant ids, bounded by
// the number of tenants the service has, not by request volume, so the map
// stays small for the life of the process and eviction would add complexity
// for no real memory benefit at this scale.
type keyedMutex struct{ m sync.Map }

// lock blocks until key's mutex is free or ctx is done, whichever comes
// first. On success it returns a func that releases the mutex; the caller is
// expected to defer it. On cancellation it returns ctx.Err() and a nil func;
// the mutex is left exactly as it was, since this caller never acquired it.
func (k *keyedMutex) lock(ctx context.Context, key string) (func(), error) {
	chAny, _ := k.m.LoadOrStore(key, make(chan struct{}, 1))
	ch := chAny.(chan struct{}) //nolint:forcetypeassert // this map only ever stores chan struct{}, set two lines up
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Repository is a domain.Repository backed by a pgx connection pool.
type Repository struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	// tenantLocks serializes RunInTx calls per tenant (see RunInTx's doc
	// comment for why). Its zero value is ready to use, since sync.Map needs
	// no initialization, but the field is spelled out explicitly here rather
	// than left implicit so the serialization mechanism is visible on the
	// struct, not just inside RunInTx.
	tenantLocks keyedMutex
}

// NewRepository returns a Repository that uses pool for all queries.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: sqlc.New(pool), tenantLocks: keyedMutex{}}
}

// compile-time check that Repository satisfies the domain port.
var _ domain.Repository = (*Repository)(nil)

// CreateAccount assigns an identity if a.ID is empty, validates the account, and
// inserts it.
func (r *Repository) CreateAccount(ctx context.Context, tenantID string, a *domain.Account) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if a.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("postgres: generate account id: %w", err)
		}
		a.ID = id.String()
	}
	if err := a.Validate(); err != nil {
		return err
	}
	aid, err := uuid.Parse(a.ID)
	if err != nil {
		return fmt.Errorf("postgres: parse account id: %w", err)
	}
	return r.q.CreateAccount(ctx, sqlc.CreateAccountParams{
		ID:       aid,
		TenantID: tid,
		Name:     a.Name,
		Type:     a.Type.String(),
		Currency: string(a.Currency),
	})
}

// GetAccount returns the account, or domain.ErrAccountNotFound if absent.
func (r *Repository) GetAccount(ctx context.Context, tenantID, id string) (domain.Account, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	aid, err := uuid.Parse(id)
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: parse account id: %w", err)
	}
	row, err := r.q.GetAccount(ctx, sqlc.GetAccountParams{TenantID: tid, ID: aid})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: get account: %w", err)
	}
	return accountFromRow(row.ID, row.Name, row.Type, row.Currency)
}

// ListAccounts returns up to limit of the tenant's accounts, ordered by name.
func (r *Repository) ListAccounts(ctx context.Context, tenantID string, limit int) ([]domain.Account, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	rows, err := r.q.ListAccounts(ctx, sqlc.ListAccountsParams{
		TenantID: tid,
		Limit:    int32(limit), //nolint:gosec // limit is bounded by the API layer
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list accounts: %w", err)
	}
	out := make([]domain.Account, 0, len(rows))
	for _, row := range rows {
		acct, err := accountFromRow(row.ID, row.Name, row.Type, row.Currency)
		if err != nil {
			return nil, err
		}
		out = append(out, acct)
	}
	return out, nil
}

// clearingAccountName is the reserved, deterministic name of a tenant's FX
// clearing account for currency (ADR-014): "fx.clearing.<CURRENCY>". Building
// it the same way on every call is what lets GetOrCreateClearingAccount
// resolve two calls for the same tenant and currency to the same row.
func clearingAccountName(currency domain.Currency) string {
	return "fx.clearing." + string(currency)
}

// GetOrCreateClearingAccount returns the tenant's per-currency FX clearing
// account, creating it (as a Liability, System account) on first use. The
// underlying query is INSERT ... ON CONFLICT (tenant_id, name) WHERE
// is_system DO UPDATE (a no-op update, just to force Postgres to return the
// existing row), so two callers racing to create the same tenant's first
// clearing account for a currency (including two callers in different
// processes) resolve to the same row rather than creating duplicates,
// erroring, or (the DO NOTHING version's bug) both losing the race against
// each other's snapshot and returning no row at all.
func (r *Repository) GetOrCreateClearingAccount(ctx context.Context, tenantID string, currency domain.Currency) (domain.Account, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: generate clearing account id: %w", err)
	}
	row, err := r.q.GetOrCreateClearingAccount(ctx, sqlc.GetOrCreateClearingAccountParams{
		ID:       id,
		TenantID: tid,
		Name:     clearingAccountName(currency),
		Type:     domain.Liability.String(),
		Currency: string(currency),
	})
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: get or create clearing account: %w", err)
	}
	acct, err := accountFromRow(row.ID, row.Name, row.Type, row.Currency)
	if err != nil {
		return domain.Account{}, err
	}
	acct.System = row.IsSystem
	return acct, nil
}

// CreateTransaction validates t and writes the transaction and all its postings
// atomically. It is a convenience wrapper around RunInTx for the common case of
// posting a single transaction; it inherits the SERIALIZABLE isolation and retry
// behavior. Validation happens once here, before the transaction starts, rather
// than inside the retried unit of work.
func (r *Repository) CreateTransaction(ctx context.Context, tenantID string, t *domain.Transaction) error {
	if err := t.Validate(); err != nil {
		return err
	}
	return r.RunInTx(ctx, tenantID, func(ctx context.Context, tx domain.Tx) error {
		return tx.CreateTransaction(ctx, tenantID, t)
	})
}

// RunInTx executes fn inside a SERIALIZABLE transaction, committing on success
// and rolling back on error. SERIALIZABLE can abort a transaction with a
// serialization conflict (SQLSTATE 40001), and the conflict often surfaces only
// at COMMIT, so both fn and the commit are watched and the whole unit of work is
// replayed up to maxPostAttempts times. fn must therefore be safe to run more
// than once. Each attempt acquires its own connection from the pool via
// BeginTx and releases it (Commit or Rollback) before the next attempt's
// backoff wait, exactly as if RunInTx had no notion of tenants at all: no
// connection is ever held across a backoff or across attempts.
//
// Before any of that, RunInTx acquires tenantID's lock from an in-process
// keyed mutex and holds it for every attempt, releasing it only when the
// whole call returns. This serializes same-tenant calls one at a time while
// leaving different tenants (different keys) fully concurrent. It exists
// because of the per-tenant audit hash chain (ADR-012): each attempt reads
// the tenant's latest audit row_hash and then inserts the next one, and two
// concurrent same-tenant attempts reading the same chain tail is a genuine
// read-write antidependency that SERIALIZABLE must abort. Under high
// same-tenant concurrency that repeated abort can exhaust the retry budget
// and surface as a 503.
//
// The lock is a channel-backed in-process mutex, not a database lock, and
// that is the point (see ADR-012). A waiter blocks on Go's scheduler; it has
// not acquired a database connection and never will until it is its turn to
// run an attempt, so a burst of same-tenant callers cannot exhaust the
// connection pool or starve other tenants of connections the way a lock held
// on a checked-out connection can. It also means Postgres's lock_timeout,
// which bounds how long a session will wait on a database-level lock, never
// applies here: there is no database lock wait to time out. Unlike a plain
// sync.Mutex, the wait itself respects ctx: if the caller's context is
// cancelled or times out while parked waiting for the tenant lock, lock
// returns ctx.Err() immediately instead of leaving the goroutine parked until
// the current holder finishes, so a pile of abandoned client requests cannot
// accumulate blocked goroutines under sustained same-tenant overload.
//
// This only serializes same-tenant posting within one process. go-ledger runs
// as a single instance (a VPS, not a fleet), so that is a complete fix today.
// If the service ever runs as more than one instance, two different
// instances could still race on the same tenant; the SERIALIZABLE retry loop
// above remains in place as the backstop for that case; it would simply see
// same-tenant conflicts occasionally instead of never.
func (r *Repository) RunInTx(ctx context.Context, tenantID string, fn func(context.Context, domain.Tx) error) error {
	unlock, err := r.tenantLocks.lock(ctx, tenantID)
	if err != nil {
		return err
	}
	defer unlock()

	var lastErr error
	for attempt := 0; attempt < maxPostAttempts; attempt++ {
		if attempt > 0 {
			metrics.SerializationRetries.Inc()
			// Exponential backoff with jitter lets the competing transaction
			// finish and spreads retriers out. Respect cancellation while waiting.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryBackoff(attempt)):
			}
		}

		tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return fmt.Errorf("postgres: begin: %w", err)
		}

		if err := fn(ctx, txRepo{q: r.q.WithTx(tx)}); err != nil {
			// Roll back with a detached context so cleanup still runs even if the
			// caller's context was cancelled (timeout, client gone).
			_ = tx.Rollback(context.WithoutCancel(ctx))
			if isSerializationFailure(err) {
				lastErr = err
				continue
			}
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			if isSerializationFailure(err) {
				lastErr = err
				continue
			}
			// The deferred balance trigger fires at COMMIT for an unbalanced
			// transaction (normally caught by Validate first, but map it anyway).
			if pgConstraint(err) == "postings_balanced" {
				return domain.ErrUnbalanced
			}
			return fmt.Errorf("postgres: commit: %w", err)
		}
		return nil
	}
	// Exhausted: surface a typed, transient error so transport can map it to 503
	// rather than 500. The underlying SQLSTATE is included for logs.
	return fmt.Errorf("postgres: serialization retries exhausted after %d attempts (%v): %w",
		maxPostAttempts, lastErr, domain.ErrConflict)
}

// isSerializationFailure reports whether err is a Postgres serialization failure
// (40001) or deadlock (40P01), the two conditions worth retrying.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

// isUniqueViolation reports whether err is a Postgres unique-violation (23505),
// for example inserting a transaction with an id that already exists.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// IsUniqueViolationError reports whether err is a Postgres unique-violation
// (23505). Exported so a caller outside this package (cmd/server's idempotent
// API key provisioning, see ADR-012) can treat "a row with this key already
// exists" as success rather than an error, without duplicating the pgconn
// error-code check.
func IsUniqueViolationError(err error) bool {
	return isUniqueViolation(err)
}

// pgConstraint returns the constraint name on a Postgres error, or "". The
// invariant triggers tag their exceptions with a constraint name (migration
// 0005), so the adapter can translate them to typed domain errors.
func pgConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// txRepo is a domain.Tx bound to one transaction's sqlc queries. All writes it
// performs are part of the surrounding pgx transaction opened by RunInTx.
type txRepo struct {
	q *sqlc.Queries
}

var _ domain.Tx = txRepo{}

// CreateTransaction assigns identities where empty and inserts the transaction
// and its postings using the bound transaction's queries. It trusts that t is
// already valid: the public entry points (Repository.CreateTransaction and the
// service layer) validate once before the transaction starts, so validation is
// not repeated here on every retry.
func (tr txRepo) CreateTransaction(ctx context.Context, tenantID string, t *domain.Transaction) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if t.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("postgres: generate transaction id: %w", err)
		}
		t.ID = id.String()
	}
	txID, err := uuid.Parse(t.ID)
	if err != nil {
		return fmt.Errorf("postgres: parse transaction id: %w", err)
	}
	// currency now lives on each posting, not on the transaction (ADR-014): an
	// FX transaction spans two currencies, so there is no single value to
	// insert here. The fx_* snapshot columns are all nullable: t.FX is nil for
	// a plain, single-currency transaction, and every field below stays its
	// zero (invalid, i.e. NULL) pgtype value in that case.
	params := sqlc.CreateTransactionParams{ID: txID, TenantID: tid}
	if t.FX != nil {
		params.FxSourceAmount = pgtype.Int8{Int64: t.FX.SourceAmount, Valid: true}
		params.FxConvertedAmount = pgtype.Int8{Int64: t.FX.ConvertedAmount, Valid: true}
		params.FxMidRateE8 = pgtype.Int8{Int64: t.FX.MidRateE8, Valid: true}
		params.FxSpreadBps = pgtype.Int4{Int32: t.FX.SpreadBps, Valid: true}
		params.FxAppliedE8 = pgtype.Int8{Int64: t.FX.AppliedE8, Valid: true}
		params.FxRateSource = pgtype.Text{String: t.FX.RateSource, Valid: true}
		params.FxEffectiveAt = pgtype.Timestamptz{Time: t.FX.EffectiveAt, Valid: true}
		params.FxRateID = pgtype.Int8{Int64: t.FX.RateID, Valid: true}
	}
	if err := tr.q.CreateTransaction(ctx, params); err != nil {
		if isUniqueViolation(err) {
			return domain.ErrDuplicateTransaction
		}
		return fmt.Errorf("postgres: insert transaction: %w", err)
	}
	for i := range t.Postings {
		p := &t.Postings[i]
		aid, err := uuid.Parse(p.AccountID)
		if err != nil {
			return fmt.Errorf("postgres: parse posting account id: %w", err)
		}
		pid, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("postgres: generate posting id: %w", err)
		}
		if err := tr.q.CreatePosting(ctx, sqlc.CreatePostingParams{
			ID:            pid,
			TenantID:      tid,
			TransactionID: txID,
			AccountID:     aid,
			Amount:        p.Amount.Amount(),
			Currency:      string(p.Amount.Currency()),
			Description:   p.Description,
		}); err != nil {
			// The currency-integrity trigger rejects a posting into an account
			// whose currency differs from the transaction's.
			if pgConstraint(err) == "postings_currency_matches" {
				return domain.ErrCurrencyMismatch
			}
			return fmt.Errorf("postgres: insert posting: %w", err)
		}
	}
	return nil
}

// InsertIdempotencyKey records the key inside the surrounding transaction. A
// primary-key collision means the key already exists: it is mapped to
// ErrDuplicateIdempotencyKey so the service can replay the original response.
func (tr txRepo) InsertIdempotencyKey(ctx context.Context, tenantID, key, fingerprint, transactionID string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	txID, err := uuid.Parse(transactionID)
	if err != nil {
		return fmt.Errorf("postgres: parse transaction id: %w", err)
	}
	if err := tr.q.InsertIdempotencyKey(ctx, sqlc.InsertIdempotencyKeyParams{
		TenantID:       tid,
		IdempotencyKey: key,
		Fingerprint:    fingerprint,
		TransactionID:  txID,
	}); err != nil {
		if pgConstraint(err) == "idempotency_keys_pkey" {
			return domain.ErrDuplicateIdempotencyKey
		}
		return fmt.Errorf("postgres: insert idempotency key: %w", err)
	}
	return nil
}

// AppendAudit writes one audit row inside the surrounding transaction,
// extending that tenant's tamper-evident hash chain (ADR-012). The id is a
// fresh UUIDv7 so rows sort by creation time.
//
// The chain extension happens entirely within this call, inside the caller's
// transaction: it reads the tenant's current latest row_hash (GetLastAuditHash;
// no rows yet means this is the tenant's first row, so prev is
// domain.AuditGenesisHash), stamps CreatedAt with the application clock (not a
// database default, so the exact value hashed is the exact value stored),
// computes RowHash over that content plus prev, and inserts all of it
// together. Because the read and the write are in the same SERIALIZABLE
// transaction, two concurrent posts for the same tenant would conflict on
// this read (one sees the other's insert as a predicate change) were they
// allowed to race at all. RunInTx prevents the race up front instead: it
// holds tenantID's in-process mutex for the whole call, so only one posting
// transaction per tenant is ever open at a time, and this read always sees
// the tenant's true latest row. See RunInTx's doc comment and ADR-012 for why
// an in-process mutex, not a database lock, is what makes that true.
func (tr txRepo) AppendAudit(ctx context.Context, tenantID string, e domain.AuditEntry) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	txID, err := uuid.Parse(e.TransactionID)
	if err != nil {
		return fmt.Errorf("postgres: parse audit transaction id: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("postgres: generate audit id: %w", err)
	}

	prevHash := domain.AuditGenesisHash
	last, err := tr.q.GetLastAuditHash(ctx, tid)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No prior row for this tenant: genesis.
	case err != nil:
		return fmt.Errorf("postgres: get last audit hash: %w", err)
	default:
		// A pre-migration legacy row has a NULL row_hash, which surfaces here
		// as an invalid pgtype.Text. Those rows predate the hash chain and are
		// cleared by the seeder's reset within four hours, so treating them as
		// an unchained starting point (genesis) is the only meaningful choice.
		// Made explicit rather than relying on the zero-value .String of an
		// invalid pgtype.Text happening to equal genesis ("").
		if last.Valid {
			prevHash = last.String
		} else {
			prevHash = domain.AuditGenesisHash
		}
	}

	e.ID = id.String()
	// Truncated to microseconds: Postgres timestamptz only stores microsecond
	// precision, so a nanosecond-precision time.Now() would silently lose its
	// last three digits on the round trip through the column. Truncating here,
	// before it is both hashed and stored, guarantees the value fed to
	// ComputeAuditRowHash is bit-for-bit the same value a later read (and thus
	// a later recompute, for the verify walk) will see; skipping this step
	// would make every stored row_hash permanently unrecomputable.
	e.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	e.PrevHash = prevHash
	e.RowHash = domain.ComputeAuditRowHash(tenantID, e, prevHash)

	if err := tr.q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		ID:            id,
		TenantID:      tid,
		Action:        e.Action,
		TransactionID: txID,
		Actor:         e.Actor,
		Before:        e.Before,
		After:         e.After,
		CreatedAt:     e.CreatedAt,
		PrevHash:      pgtype.Text{String: e.PrevHash, Valid: true},
		RowHash:       pgtype.Text{String: e.RowHash, Valid: true},
	}); err != nil {
		return fmt.Errorf("postgres: insert audit log: %w", err)
	}
	return nil
}

// GetTransaction returns the transaction and its postings, or
// domain.ErrTransactionNotFound if absent.
func (r *Repository) GetTransaction(ctx context.Context, tenantID, id string) (domain.Transaction, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	txID, err := uuid.Parse(id)
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("postgres: parse transaction id: %w", err)
	}
	row, err := r.q.GetTransaction(ctx, sqlc.GetTransactionParams{TenantID: tid, ID: txID})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Transaction{}, domain.ErrTransactionNotFound
	}
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("postgres: get transaction: %w", err)
	}
	postings, err := r.q.ListPostingsByTransaction(ctx, sqlc.ListPostingsByTransactionParams{
		TenantID:      tid,
		TransactionID: txID,
	})
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("postgres: list postings: %w", err)
	}
	out := domain.Transaction{ID: row.ID.String(), Postings: make([]domain.Posting, 0, len(postings))}
	for _, p := range postings {
		// Each posting carries its own currency (ADR-014): an FX transaction has
		// two currencies in play, so Money is rebuilt per row, never from one
		// transaction-wide currency.
		money, err := domain.NewMoney(p.Amount, domain.Currency(p.Currency))
		if err != nil {
			return domain.Transaction{}, fmt.Errorf("postgres: build posting money: %w", err)
		}
		out.Postings = append(out.Postings, domain.Posting{
			AccountID:   p.AccountID.String(),
			Amount:      money,
			Description: p.Description,
		})
	}
	// fx_mid_rate_e8 is only ever NULL together with the other seven fx_*
	// columns (all written in the same CreateTransaction call, see txRepo);
	// its validity is enough to tell an FX transaction from a plain one.
	if row.FxMidRateE8.Valid {
		out.FX = &domain.FXDetail{
			SourceAmount:    row.FxSourceAmount.Int64,
			ConvertedAmount: row.FxConvertedAmount.Int64,
			MidRateE8:       row.FxMidRateE8.Int64,
			AppliedE8:       row.FxAppliedE8.Int64,
			SpreadBps:       row.FxSpreadBps.Int32,
			RateSource:      row.FxRateSource.String,
			EffectiveAt:     row.FxEffectiveAt.Time,
			RateID:          row.FxRateID.Int64,
		}
	}
	return out, nil
}

// GetIdempotencyKey returns the stored record for (tenantID, key), or
// domain.ErrIdempotencyKeyNotFound if none exists.
func (r *Repository) GetIdempotencyKey(ctx context.Context, tenantID, key string) (domain.IdempotencyRecord, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.IdempotencyRecord{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	row, err := r.q.GetIdempotencyKey(ctx, sqlc.GetIdempotencyKeyParams{TenantID: tid, IdempotencyKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.IdempotencyRecord{}, domain.ErrIdempotencyKeyNotFound
	}
	if err != nil {
		return domain.IdempotencyRecord{}, fmt.Errorf("postgres: get idempotency key: %w", err)
	}
	return domain.IdempotencyRecord{
		Key:           row.IdempotencyKey,
		Fingerprint:   row.Fingerprint,
		TransactionID: row.TransactionID.String(),
	}, nil
}

// ListAuditByTransaction returns the audit rows for a transaction, oldest first.
func (r *Repository) ListAuditByTransaction(ctx context.Context, tenantID, transactionID string) ([]domain.AuditEntry, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	txID, err := uuid.Parse(transactionID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse transaction id: %w", err)
	}
	rows, err := r.q.ListAuditByTransaction(ctx, sqlc.ListAuditByTransactionParams{TenantID: tid, TransactionID: txID})
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit by transaction: %w", err)
	}
	return auditEntriesFromRows(rows), nil
}

// ListAuditByAccount returns one keyset page of audit rows for every
// transaction with a posting touching the account, newest first.
func (r *Repository) ListAuditByAccount(ctx context.Context, tenantID, accountID string, after *domain.StatementCursor, limit int) ([]domain.AuditEntry, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	aid, err := uuid.Parse(accountID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse account id: %w", err)
	}

	// First page: a sentinel that is strictly greater than any real (created_at,
	// id). Subsequent pages: the cursor handed back from the previous page.
	afterTime, afterID := statementFirstPageTime, uuid.Max
	if after != nil {
		afterTime = after.CreatedAt
		if afterID, err = uuid.Parse(after.ID); err != nil {
			return nil, fmt.Errorf("postgres: parse cursor id: %w", err)
		}
	}

	rows, err := r.q.ListAuditByAccount(ctx, sqlc.ListAuditByAccountParams{
		TenantID:       tid,
		AccountID:      aid,
		AfterCreatedAt: afterTime,
		AfterID:        afterID,
		PageLimit:      int32(limit), //nolint:gosec // limit is bounded by the API layer
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit by account: %w", err)
	}
	return auditEntriesFromRows(rows), nil
}

// ListAuditForVerify returns every audit row for the tenant, oldest first,
// including PrevHash and RowHash: the full walk used to recompute and check
// the tamper-evident hash chain end to end.
func (r *Repository) ListAuditForVerify(ctx context.Context, tenantID string) ([]domain.AuditEntry, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	rows, err := r.q.ListAuditForVerify(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit for verify: %w", err)
	}
	return auditEntriesFromRows(rows), nil
}

// auditEntriesFromRows converts sqlc audit rows to domain entries. Before/After
// are jsonb columns surfaced as []byte; they convert to json.RawMessage.
// PrevHash/RowHash are nullable at the column level only for rows written
// before migration 0009; every row this application writes populates both, and
// .String on an invalid (NULL) pgtype.Text zero-values to "" for those legacy
// rows.
func auditEntriesFromRows(rows []sqlc.AuditLog) []domain.AuditEntry {
	out := make([]domain.AuditEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.AuditEntry{
			ID:            row.ID.String(),
			Action:        row.Action,
			TransactionID: row.TransactionID.String(),
			Actor:         row.Actor,
			Before:        row.Before,
			After:         row.After,
			CreatedAt:     row.CreatedAt,
			PrevHash:      row.PrevHash.String,
			RowHash:       row.RowHash.String,
		})
	}
	return out
}

// Balance returns the derived balance of an account in the account's currency.
func (r *Repository) Balance(ctx context.Context, tenantID, accountID string) (domain.Money, error) {
	// Read the account first: it is the source of the currency and lets us
	// distinguish "no postings yet" (balance zero) from "no such account".
	acct, err := r.GetAccount(ctx, tenantID, accountID)
	if err != nil {
		return domain.Money{}, err
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.Money{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	aid, err := uuid.Parse(accountID)
	if err != nil {
		return domain.Money{}, fmt.Errorf("postgres: parse account id: %w", err)
	}
	sum, err := r.q.AccountBalance(ctx, sqlc.AccountBalanceParams{TenantID: tid, AccountID: aid})
	if err != nil {
		return domain.Money{}, fmt.Errorf("postgres: account balance: %w", err)
	}
	return domain.NewMoney(sum, acct.Currency)
}

// statementFirstPageTime is the far-future keyset sentinel used to fetch the
// newest page (every real created_at is strictly before it).
var statementFirstPageTime = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// Statement returns one keyset page of the account's postings, newest first,
// each with its running balance, in the given currency.
func (r *Repository) Statement(ctx context.Context, tenantID, accountID string, currency domain.Currency, after *domain.StatementCursor, limit int) ([]domain.StatementEntry, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	aid, err := uuid.Parse(accountID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse account id: %w", err)
	}

	// First page: a sentinel that is strictly greater than any real (created_at,
	// id). Subsequent pages: the cursor handed back from the previous page.
	afterTime, afterID := statementFirstPageTime, uuid.Max
	if after != nil {
		afterTime = after.CreatedAt
		if afterID, err = uuid.Parse(after.ID); err != nil {
			return nil, fmt.Errorf("postgres: parse cursor id: %w", err)
		}
	}

	rows, err := r.q.AccountStatement(ctx, sqlc.AccountStatementParams{
		TenantID:       tid,
		AccountID:      aid,
		AfterCreatedAt: afterTime,
		AfterID:        afterID,
		PageLimit:      int32(limit), //nolint:gosec // limit is bounded by the API layer
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: account statement: %w", err)
	}

	entries := make([]domain.StatementEntry, 0, len(rows))
	for _, row := range rows {
		amount, err := domain.NewMoney(row.Amount, currency)
		if err != nil {
			return nil, fmt.Errorf("postgres: statement amount: %w", err)
		}
		running, err := domain.NewMoney(row.RunningBalance, currency)
		if err != nil {
			return nil, fmt.Errorf("postgres: statement running balance: %w", err)
		}
		entries = append(entries, domain.StatementEntry{
			ID:             row.ID.String(),
			TransactionID:  row.TransactionID.String(),
			Amount:         amount,
			RunningBalance: running,
			Description:    row.Description,
			CreatedAt:      row.CreatedAt,
		})
	}
	return entries, nil
}

// GetAPIKeyByHash resolves an unrevoked api_keys row by hash, or
// domain.ErrAPIKeyNotFound if none exists.
func (r *Repository) GetAPIKeyByHash(ctx context.Context, hash string) (domain.APIKey, error) {
	row, err := r.q.GetAPIKeyByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.APIKey{}, domain.ErrAPIKeyNotFound
	}
	if err != nil {
		return domain.APIKey{}, fmt.Errorf("postgres: get api key by hash: %w", err)
	}
	return domain.APIKey{
		ID:           row.ID.String(),
		TenantID:     row.TenantID.String(),
		Name:         row.Name,
		RateLimitRPM: int4ToPtr(row.RateLimitRpm),
	}, nil
}

// InsertAPIKey assigns an identity if k.ID is empty and inserts k with keyHash
// as its stored credential. Only the hash is ever written.
func (r *Repository) InsertAPIKey(ctx context.Context, k domain.APIKey, keyHash string) error {
	if k.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("postgres: generate api key id: %w", err)
		}
		k.ID = id.String()
	}
	id, err := uuid.Parse(k.ID)
	if err != nil {
		return fmt.Errorf("postgres: parse api key id: %w", err)
	}
	tid, err := uuid.Parse(k.TenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if err := r.q.InsertAPIKey(ctx, sqlc.InsertAPIKeyParams{
		ID:           id,
		TenantID:     tid,
		Name:         k.Name,
		KeyHash:      keyHash,
		RateLimitRpm: ptrToInt4(k.RateLimitRPM),
	}); err != nil {
		return fmt.Errorf("postgres: insert api key: %w", err)
	}
	return nil
}

// int4ToPtr converts a nullable Postgres int4 to *int, nil when the column is
// NULL (no per-key rate limit override).
func int4ToPtr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}

// ptrToInt4 converts *int to a nullable Postgres int4, NULL when p is nil.
func ptrToInt4(p *int) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*p), Valid: true} //nolint:gosec // rate limits are small, application-set values
}

// accountFromRow builds a domain.Account from the scalar fields common to every
// account-shaped row sqlc generates (GetAccountRow, ListAccountsRow, ...). Taking
// individual fields rather than one sqlc row type is deliberate: sqlc gives each
// query its own row struct whenever its column list doesn't exactly match every
// column of the accounts table (which stopped being true once the schema grew
// columns like is_system that not every query selects), so a single shared
// struct type would not compile across call sites.
func accountFromRow(id uuid.UUID, name, accountType, currency string) (domain.Account, error) {
	at, err := domain.ParseAccountType(accountType)
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: parse account type: %w", err)
	}
	return domain.Account{
		ID:       id.String(),
		Name:     name,
		Type:     at,
		Currency: domain.Currency(currency),
	}, nil
}
