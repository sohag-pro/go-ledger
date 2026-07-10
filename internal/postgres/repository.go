// Package postgres is the Postgres storage adapter for the ledger. It implements
// the domain.Repository port on top of pgx and sqlc-generated queries. It holds
// no business rules: the double-entry invariant is enforced by the domain
// (Transaction.Validate) and, from Week 4, by a database CHECK constraint.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
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

// Repository is a domain.Repository backed by a pgx connection pool.
type Repository struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

// NewRepository returns a Repository that uses pool for all queries.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: sqlc.New(pool)}
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
// backoff wait: no connection is ever held across a backoff or across
// attempts.
//
// Until ADR-017, RunInTx also acquired tenantID's lock from an in-process
// keyed mutex before opening any transaction, and held it for every attempt:
// each post read the tenant's latest audit row_hash and then inserted the
// next one (ADR-012), and two concurrent same-tenant attempts racing on that
// read were a genuine read-write antidependency that SERIALIZABLE would
// abort, which under high same-tenant concurrency could repeatedly exhaust
// the retry budget and surface as a 503. Worse, that mutex lived in one
// process's memory: with more than one instance, two instances could still
// post the same tenant concurrently, both read the same chain head, and fork
// the chain, since SERIALIZABLE is not guaranteed to see every such conflict
// across processes (ADR-017's Context).
//
// ADR-017 removes the audit chain read from the posting transaction
// entirely: a post now writes an append-only outbox row (see
// domain.Tx.AppendAuditOutbox), and a single background chainer
// (internal/audit.Chainer) builds the chain asynchronously, so no posting
// transaction ever reads or extends a chain head. That removes the reason
// for the mutex, so RunInTx no longer takes one: same-tenant calls, from any
// number of instances, now run fully concurrently, serialized only by
// whatever SERIALIZABLE itself detects (the balance invariant and the
// idempotency primary key, both still enforced in-transaction) and retried
// exactly as any other serialization conflict is.
func (r *Repository) RunInTx(ctx context.Context, tenantID string, fn func(context.Context, domain.Tx) error) error { //nolint:revive // tenantID is part of domain.Repository's interface signature and kept named for godoc; ADR-017 removed the per-tenant mutex that used to read it here
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
// scheme is stored alongside fingerprint (see domain.CurrentFingerprintScheme)
// so a future fingerprint-scheme change can recompute this row's fingerprint
// under the scheme that produced it instead of the scheme current at replay
// time.
func (tr txRepo) InsertIdempotencyKey(ctx context.Context, tenantID, key, fingerprint, scheme, transactionID string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	txID, err := uuid.Parse(transactionID)
	if err != nil {
		return fmt.Errorf("postgres: parse transaction id: %w", err)
	}
	if err := tr.q.InsertIdempotencyKey(ctx, sqlc.InsertIdempotencyKeyParams{
		TenantID:          tid,
		IdempotencyKey:    key,
		Fingerprint:       fingerprint,
		FingerprintScheme: scheme,
		TransactionID:     txID,
	}); err != nil {
		if pgConstraint(err) == "idempotency_keys_pkey" {
			return domain.ErrDuplicateIdempotencyKey
		}
		return fmt.Errorf("postgres: insert idempotency key: %w", err)
	}
	return nil
}

// AppendAuditOutbox writes one append-only audit_outbox row inside the
// surrounding transaction (ADR-017): the event is durable if and only if the
// caller's transaction commits. Unlike the old AppendAudit, it never reads
// the tenant's chain head and never computes a hash: it is a plain insert
// with no read dependency on any other row, tenant or otherwise, so it
// cannot conflict with a concurrent same-tenant (or same-anything) insert
// under SERIALIZABLE. occurred_at, txid, and created_at are all left to
// their column defaults (migration 0015); the database server's clock and
// current transaction id stamp them, not this process's.
//
// The single background chainer (internal/audit.Chainer) is what later reads
// this row back and extends the tenant's tamper-evident hash chain.
func (tr txRepo) AppendAuditOutbox(ctx context.Context, tenantID string, e domain.AuditEvent) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	txID, err := uuid.Parse(e.TransactionID)
	if err != nil {
		return fmt.Errorf("postgres: parse audit transaction id: %w", err)
	}
	if err := tr.q.InsertAuditOutbox(ctx, sqlc.InsertAuditOutboxParams{
		TenantID:      tid,
		Action:        e.Action,
		TransactionID: txID,
		Actor:         e.Actor,
		Before:        e.Before,
		After:         e.After,
	}); err != nil {
		return fmt.Errorf("postgres: insert audit outbox: %w", err)
	}
	return nil
}

// TenantDailyDebits returns the tenant's per-currency debit total for today,
// within the surrounding transaction (Task 2.4b, audit A3.4). See
// domain.Tx.TenantDailyDebits for the race-safety this depends on: RunInTx's
// per-tenant in-process lock (ADR-012) is what makes this read consistent
// with the write that follows it in the same call.
func (tr txRepo) TenantDailyDebits(ctx context.Context, tenantID string) (map[string]int64, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	rows, err := tr.q.TenantDailyDebits(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("postgres: tenant daily debits: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Currency] = row.Total
	}
	return out, nil
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
		Scheme:        row.FingerprintScheme,
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
	return out, nil
}

// ListAuditByAccount returns one keyset page of audit rows for every
// transaction with a posting touching the account, newest first.
//
// Paging keys on id alone, not (created_at, id) (ADR-017): id is the
// chainer's true chain-insertion order, and created_at (copied from the
// originating event's post time) is not guaranteed monotonic with that order
// under concurrent posts (see GetLastAuditHash's doc comment in
// internal/postgres/queries/audit.sql). after.CreatedAt is accepted (the
// domain.StatementCursor type is shared with Statement's own, unrelated
// posting pagination) but not used here beyond round-tripping through the
// cursor: only after.ID drives the query.
func (r *Repository) ListAuditByAccount(ctx context.Context, tenantID, accountID string, after *domain.StatementCursor, limit int) ([]domain.AuditEntry, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	aid, err := uuid.Parse(accountID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse account id: %w", err)
	}

	// First page: a sentinel strictly greater than any real id. Subsequent
	// pages: the cursor handed back from the previous page.
	afterID := uuid.Max
	if after != nil {
		if afterID, err = uuid.Parse(after.ID); err != nil {
			return nil, fmt.Errorf("postgres: parse cursor id: %w", err)
		}
	}

	rows, err := r.q.ListAuditByAccount(ctx, sqlc.ListAuditByAccountParams{
		TenantID:  tid,
		AccountID: aid,
		AfterID:   afterID,
		PageLimit: int32(limit), //nolint:gosec // limit is bounded by the API layer
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit by account: %w", err)
	}
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
	return out, nil
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
	return out, nil
}

// CountPendingOutbox returns the number of tenantID's audit_outbox rows the
// chainer has not yet processed (ADR-017): the audit chain's lag, surfaced by
// audit verify alongside the chained head.
func (r *Repository) CountPendingOutbox(ctx context.Context, tenantID string) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	n, err := r.q.CountPendingOutbox(ctx, tid)
	if err != nil {
		return 0, fmt.Errorf("postgres: count pending outbox: %w", err)
	}
	return int(n), nil
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
		TenantStatus: domain.TenantStatus(row.TenantStatus),
		Scopes:       scopesFromStrings(row.Scopes),
		ExpiresAt:    timestamptzToPtr(row.ExpiresAt),
		LastUsedAt:   timestamptzToPtr(row.LastUsedAt),
		CreatedAt:    row.CreatedAt,
		RevokedAt:    timestamptzToPtr(row.RevokedAt),
	}, nil
}

// GetAPIKeyByID returns the api_keys row with the given id, revoked or not,
// or domain.ErrAPIKeyNotFound if none exists (Task 2.2b).
func (r *Repository) GetAPIKeyByID(ctx context.Context, id string) (domain.APIKey, error) {
	keyID, err := uuid.Parse(id)
	if err != nil {
		return domain.APIKey{}, fmt.Errorf("postgres: parse api key id: %w", err)
	}
	row, err := r.q.GetAPIKeyByID(ctx, keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.APIKey{}, domain.ErrAPIKeyNotFound
	}
	if err != nil {
		return domain.APIKey{}, fmt.Errorf("postgres: get api key by id: %w", err)
	}
	return domain.APIKey{
		ID:           row.ID.String(),
		TenantID:     row.TenantID.String(),
		Name:         row.Name,
		RateLimitRPM: int4ToPtr(row.RateLimitRpm),
		Scopes:       scopesFromStrings(row.Scopes),
		ExpiresAt:    timestamptzToPtr(row.ExpiresAt),
		LastUsedAt:   timestamptzToPtr(row.LastUsedAt),
		CreatedAt:    row.CreatedAt,
		RevokedAt:    timestamptzToPtr(row.RevokedAt),
	}, nil
}

// ListAPIKeysByTenant returns every api_keys row for tenantID, oldest first,
// revoked or not (Task 2.2b).
func (r *Repository) ListAPIKeysByTenant(ctx context.Context, tenantID string) ([]domain.APIKey, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	rows, err := r.q.ListAPIKeysByTenant(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("postgres: list api keys by tenant: %w", err)
	}
	out := make([]domain.APIKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.APIKey{
			ID:           row.ID.String(),
			TenantID:     row.TenantID.String(),
			Name:         row.Name,
			RateLimitRPM: int4ToPtr(row.RateLimitRpm),
			Scopes:       scopesFromStrings(row.Scopes),
			ExpiresAt:    timestamptzToPtr(row.ExpiresAt),
			LastUsedAt:   timestamptzToPtr(row.LastUsedAt),
			CreatedAt:    row.CreatedAt,
			RevokedAt:    timestamptzToPtr(row.RevokedAt),
		})
	}
	return out, nil
}

// RevokeAPIKey sets revoked_at (if not already set) for the key identified by
// id (Task 2.2b), or returns domain.ErrAPIKeyNotFound if no key matches id.
func (r *Repository) RevokeAPIKey(ctx context.Context, id string) error {
	keyID, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("postgres: parse api key id: %w", err)
	}
	rows, err := r.q.RevokeAPIKey(ctx, keyID)
	if err != nil {
		return fmt.Errorf("postgres: revoke api key: %w", err)
	}
	if rows == 0 {
		return domain.ErrAPIKeyNotFound
	}
	return nil
}

// TouchAPIKeyLastUsed sets api_keys.last_used_at for the key identified by
// id. Called best-effort and throttled from the auth resolver (Task 2.2), so
// its caller (internal/auth.Resolver) fires it asynchronously and ignores its
// error beyond a debug log: a failed touch must never fail the request it
// rode in on.
func (r *Repository) TouchAPIKeyLastUsed(ctx context.Context, id string, when time.Time) error {
	keyID, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("postgres: parse api key id: %w", err)
	}
	if err := r.q.TouchAPIKeyLastUsed(ctx, sqlc.TouchAPIKeyLastUsedParams{
		ID:         keyID,
		LastUsedAt: pgtype.Timestamptz{Time: when, Valid: true},
	}); err != nil {
		return fmt.Errorf("postgres: touch api key last used: %w", err)
	}
	return nil
}

// scopesFromStrings converts the raw text[] scopes column to []domain.Scope.
// It does not validate each element against Scope.Valid(): the
// api_keys_scopes_valid CHECK constraint (migration 0012) already guarantees
// every stored value is one of the three known scopes.
func scopesFromStrings(scopes []string) []domain.Scope {
	if scopes == nil {
		return nil
	}
	out := make([]domain.Scope, len(scopes))
	for i, s := range scopes {
		out[i] = domain.Scope(s)
	}
	return out
}

// timestamptzToPtr converts a nullable Postgres timestamptz to *time.Time,
// nil when the column is NULL (never expires, or never yet used).
func timestamptzToPtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

// InsertAPIKey assigns an identity if k.ID is empty and inserts k with keyHash
// as its stored credential. Only the hash is ever written. An empty k.Scopes
// defaults to {read, post} (Task 2.2b), matching the api_keys.scopes column's
// own default, so every caller that predates scopes (cmd/server's demo and
// load-test provisioning, and every pre-2.2 test) keeps working unchanged
// instead of hitting the scopes NOT NULL / api_keys_scopes_valid constraint.
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
	scopes := k.Scopes
	if len(scopes) == 0 {
		scopes = []domain.Scope{domain.ScopeRead, domain.ScopePost}
	}
	if err := r.q.InsertAPIKey(ctx, sqlc.InsertAPIKeyParams{
		ID:           id,
		TenantID:     tid,
		Name:         k.Name,
		KeyHash:      keyHash,
		RateLimitRpm: ptrToInt4(k.RateLimitRPM),
		Scopes:       scopesToStrings(scopes),
		ExpiresAt:    ptrToTimestamptz(k.ExpiresAt),
	}); err != nil {
		return fmt.Errorf("postgres: insert api key: %w", err)
	}
	return nil
}

// scopesToStrings converts []domain.Scope to the raw []string the scopes
// text[] column stores, the reverse of scopesFromStrings.
func scopesToStrings(scopes []domain.Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// ptrToTimestamptz converts *time.Time to a nullable Postgres timestamptz,
// NULL when p is nil, the reverse of timestamptzToPtr.
func ptrToTimestamptz(p *time.Time) pgtype.Timestamptz {
	if p == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *p, Valid: true}
}

// CreateTenant inserts a new tenant row with the given id and name, active by
// default (the column default; this method never sets status explicitly). It
// returns domain.ErrTenantAlreadyExists if id is already in use.
func (r *Repository) CreateTenant(ctx context.Context, tenantID, name string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if err := (domain.Tenant{Name: name, Status: domain.TenantActive}).Validate(); err != nil {
		return err
	}
	if err := r.q.CreateTenant(ctx, sqlc.CreateTenantParams{ID: tid, Name: name}); err != nil {
		if isUniqueViolation(err) {
			return domain.ErrTenantAlreadyExists
		}
		return fmt.Errorf("postgres: create tenant: %w", err)
	}
	return nil
}

// GetTenant returns the tenant with the given id, or domain.ErrTenantNotFound
// if none exists.
func (r *Repository) GetTenant(ctx context.Context, tenantID string) (domain.Tenant, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.Tenant{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	row, err := r.q.GetTenant(ctx, tid)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Tenant{}, domain.ErrTenantNotFound
	}
	if err != nil {
		return domain.Tenant{}, fmt.Errorf("postgres: get tenant: %w", err)
	}
	return tenantFromRow(row), nil
}

// ListTenants returns up to limit tenants, oldest first.
func (r *Repository) ListTenants(ctx context.Context, limit int) ([]domain.Tenant, error) {
	rows, err := r.q.ListTenants(ctx, int32(limit)) //nolint:gosec // limit is bounded by the caller
	if err != nil {
		return nil, fmt.Errorf("postgres: list tenants: %w", err)
	}
	out := make([]domain.Tenant, 0, len(rows))
	for _, row := range rows {
		out = append(out, tenantFromRow(row))
	}
	return out, nil
}

// SetTenantStatus updates the tenant's status. It returns domain.ErrInvalidTenant
// if status is not one of TenantStatus.Valid()'s three values, or
// domain.ErrTenantNotFound if no tenant matches id.
func (r *Repository) SetTenantStatus(ctx context.Context, tenantID string, status domain.TenantStatus) error {
	if !status.Valid() {
		return domain.ErrInvalidTenant
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	rows, err := r.q.SetTenantStatus(ctx, sqlc.SetTenantStatusParams{ID: tid, Status: string(status)})
	if err != nil {
		return fmt.Errorf("postgres: set tenant status: %w", err)
	}
	if rows == 0 {
		return domain.ErrTenantNotFound
	}
	return nil
}

// SetTenantSettings overwrites the tenant's settings jsonb column with
// settings (Task 2.4b, audit A3.4): a whole-document replace, not a merge
// (see domain.Repository.SetTenantSettings). It returns
// domain.ErrTenantNotFound if no tenant matches id.
func (r *Repository) SetTenantSettings(ctx context.Context, tenantID string, settings json.RawMessage) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	rows, err := r.q.SetTenantSettings(ctx, sqlc.SetTenantSettingsParams{ID: tid, Settings: settings})
	if err != nil {
		return fmt.Errorf("postgres: set tenant settings: %w", err)
	}
	if rows == 0 {
		return domain.ErrTenantNotFound
	}
	return nil
}

// InsertFXRate appends a new fx_rates row (Task 2.4, audit A3.3). tenantID
// nil inserts the global default rate (tenant_id NULL); a non-nil tenantID
// inserts that tenant's own rate, after confirming the tenant exists (a
// clean domain.ErrTenantNotFound rather than a raw foreign-key-violation
// error from the fx_rates_tenant_id_fkey constraint).
//
// Validation mirrors the fx_rates CHECK constraints and runs before any
// write, the same defense-in-depth style CreateAccount and CreateTransaction
// use elsewhere in this file: base and quote must each be a valid currency
// code and must differ, midRateE8 must be positive, and spreadBps must be in
// [0, 10000).
//
// effectiveAt nil leaves the sqlc param unset (pgtype.Timestamptz{Valid:
// false}), which the InsertFXRate query's COALESCE(sqlc.narg('effective_at'),
// now()) resolves to the DATABASE SERVER's now(), not this process's clock
// (see the query's doc comment and domain.Repository.InsertFXRate for why
// that distinction is a real correctness fix, not a style choice). A non-nil
// effectiveAt (an explicit, possibly future, scheduled rate) is passed
// through untouched.
func (r *Repository) InsertFXRate(ctx context.Context, tenantID *string, base, quote domain.Currency, midRateE8 int64, spreadBps int32, source string, effectiveAt *time.Time) error {
	if err := base.Validate(); err != nil {
		return err
	}
	if err := quote.Validate(); err != nil {
		return err
	}
	if base == quote {
		return domain.ErrSameCurrencyRate
	}
	if midRateE8 <= 0 {
		return domain.ErrNonPositiveRate
	}
	if spreadBps < 0 || spreadBps >= 10_000 {
		return domain.ErrInvalidSpread
	}

	var pgTenantID pgtype.UUID
	if tenantID != nil {
		tid, err := uuid.Parse(*tenantID)
		if err != nil {
			return fmt.Errorf("postgres: parse tenant id: %w", err)
		}
		if _, err := r.q.GetTenant(ctx, tid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTenantNotFound
			}
			return fmt.Errorf("postgres: check tenant exists: %w", err)
		}
		pgTenantID = pgtype.UUID{Bytes: tid, Valid: true}
	}

	var pgEffectiveAt pgtype.Timestamptz
	if effectiveAt != nil {
		pgEffectiveAt = pgtype.Timestamptz{Time: *effectiveAt, Valid: true}
	}

	if _, err := r.q.InsertFXRate(ctx, sqlc.InsertFXRateParams{
		TenantID:    pgTenantID,
		Base:        string(base),
		Quote:       string(quote),
		MidRateE8:   midRateE8,
		SpreadBps:   spreadBps,
		Source:      source,
		EffectiveAt: pgEffectiveAt,
	}); err != nil {
		return fmt.Errorf("postgres: insert fx rate: %w", err)
	}
	return nil
}

// tenantFromRow converts a sqlc Tenant row to a domain.Tenant.
func tenantFromRow(row sqlc.Tenant) domain.Tenant {
	return domain.Tenant{
		ID:        row.ID.String(),
		Name:      row.Name,
		Status:    domain.TenantStatus(row.Status),
		Settings:  json.RawMessage(row.Settings),
		CreatedAt: row.CreatedAt,
	}
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
