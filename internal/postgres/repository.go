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

// withTenant runs fn against a *sqlc.Queries bound to a dedicated
// transaction with the RLS GUC app.tenant_id set to tenantID
// (transaction-local, migration 0024, Task 5.4b, audit A3.5), then commits.
// It exists because the app's tenant-scoped reads run directly on r.q
// (bound to the pool), which carries no GUC and therefore no RLS
// protection: a single bare statement on a pooled connection is its own
// implicit transaction, so a set_config('app.tenant_id', ..., true) issued
// as a separate statement would evaporate before the very next query ran.
// Wrapping both the set_config and the read in one explicit transaction is
// what makes the GUC actually apply while fn runs.
//
// It is also used for the handful of standalone writes that, like the
// reads above, run outside RunInTx (CreateAccount, GetOrCreateClearingAccount,
// SetAccountStatus, CreateWebhookSubscription, a tenant-specific
// InsertFXRate): the same "forgotten WHERE/mismatched value" defense in
// depth RunInTx gives every write inside a domain.Tx applies to these too.
//
// The extra per-call transaction (BEGIN, set_config, the query, COMMIT,
// versus one bare statement on the pool) is an accepted cost of the
// defense-in-depth: RLS with FORCE is only a backstop if the tenant's GUC
// is actually set on the connection that runs the query, and that requires
// a real transaction boundary.
//
// Deliberately NOT used for genuinely cross-tenant reads: the audit
// chainer, the webhook fan-out and delivery worker, the idempotency
// sweep, and restore-verify's own tenant enumeration all query through
// their own code paths (or, for the sweep, directly on r.q with no
// tenantID in scope), never through withTenant, so the GUC stays unset on
// those connections and the "allow when unset" branch of every policy
// keeps their cross-tenant access working.
func (r *Repository) withTenant(ctx context.Context, tenantID string, fn func(q *sqlc.Queries) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin tenant-scoped call: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return fmt.Errorf("postgres: set tenant guc: %w", err)
	}
	if err := fn(sqlc.New(tx)); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit tenant-scoped call: %w", err)
	}
	return nil
}

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
	// status is not accepted at creation (Task 5.5, audit A1.5): every
	// account is created active, the column default; freezing or closing one
	// afterward goes through SetAccountStatus. Stamping it here too (rather
	// than leaving a.Status "") is just so the *domain.Account this call
	// hands back to its caller already reflects "active" without a round
	// trip through GetAccount, mirroring how a.ID is assigned above.
	if a.Status == "" {
		a.Status = domain.AccountActive
	}
	if err := a.Validate(); err != nil {
		return err
	}
	aid, err := uuid.Parse(a.ID)
	if err != nil {
		return fmt.Errorf("postgres: parse account id: %w", err)
	}
	pid, err := optUUID(a.ParentID)
	if err != nil {
		return err
	}
	return r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		return mapHierarchyErr(q.CreateAccount(ctx, sqlc.CreateAccountParams{
			ID:             aid,
			TenantID:       tid,
			Name:           a.Name,
			Type:           a.Type.String(),
			Currency:       string(a.Currency),
			MinBalance:     ptrToInt8(a.MinBalance),
			PartyReference: ptrToText(a.PartyReference),
			PartyType:      ptrToText(a.PartyType),
			ParentID:       pid,
		}))
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
	var row sqlc.GetAccountRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetAccount(ctx, sqlc.GetAccountParams{TenantID: tid, ID: aid})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: get account: %w", err)
	}
	return accountFromRow(row.ID, row.Name, row.Type, row.Currency, row.Status, row.MinBalance, row.IsSystem, row.PartyReference, row.PartyType, row.ParentID)
}

// ListAccounts returns up to limit of the tenant's accounts, ordered by name.
func (r *Repository) ListAccounts(ctx context.Context, tenantID string, limit int) ([]domain.Account, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var rows []sqlc.ListAccountsRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListAccounts(ctx, sqlc.ListAccountsParams{
			TenantID: tid,
			Limit:    int32(limit), //nolint:gosec // limit is bounded by the API layer
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list accounts: %w", err)
	}
	out := make([]domain.Account, 0, len(rows))
	for _, row := range rows {
		acct, err := accountFromRow(row.ID, row.Name, row.Type, row.Currency, row.Status, row.MinBalance, row.IsSystem, row.PartyReference, row.PartyType, row.ParentID)
		if err != nil {
			return nil, err
		}
		out = append(out, acct)
	}
	return out, nil
}

// SetAccountStatus updates the account's status (Task 5.5, audit A1.5). It
// returns domain.ErrInvalidAccount if status is not one of
// AccountStatus.Valid()'s three values, or domain.ErrAccountNotFound if no
// account matches id within tenantID.
func (r *Repository) SetAccountStatus(ctx context.Context, tenantID, id string, status domain.AccountStatus) error {
	if !status.Valid() {
		return domain.ErrInvalidAccount
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	aid, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("postgres: parse account id: %w", err)
	}
	var rows int64
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.SetAccountStatus(ctx, sqlc.SetAccountStatusParams{TenantID: tid, ID: aid, Status: string(status)})
		return err
	})
	if err != nil {
		return fmt.Errorf("postgres: set account status: %w", err)
	}
	if rows == 0 {
		return domain.ErrAccountNotFound
	}
	return nil
}

// SetAccountParent sets, changes, or clears (parentID nil) accountID's parent
// (ADR-023). It returns the number of rows updated (0 if no such account).
// Cycle, currency, and same-tenant are enforced by accounts_hierarchy_guard
// and the accounts_parent_fk composite foreign key, mapped here via
// mapHierarchyErr into domain.ErrInvalidHierarchy / domain.ErrParentNotFound.
func (r *Repository) SetAccountParent(ctx context.Context, tenantID, accountID string, parentID *string) (int64, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	aid, err := uuid.Parse(accountID)
	if err != nil {
		return 0, fmt.Errorf("postgres: parse account id: %w", err)
	}
	pid, err := optUUID(parentID)
	if err != nil {
		return 0, err
	}
	var n int64
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var e error
		n, e = q.SetAccountParent(ctx, sqlc.SetAccountParentParams{
			TenantID: tid,
			ID:       aid,
			ParentID: pid,
		})
		return mapHierarchyErr(e)
	})
	return n, err
}

// RolledUpBalance returns the balance of accountID and all its descendants
// (ADR-023). It returns domain.ErrAccountNotFound if accountID does not
// exist: GetAccount is used first, both to produce that clear error (rather
// than a silent zero from an empty recursive base row) and to get the
// account's own currency for the returned Money.
func (r *Repository) RolledUpBalance(ctx context.Context, tenantID, accountID string) (domain.Money, error) {
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
	var amount int64
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var e error
		amount, e = q.RolledUpBalance(ctx, sqlc.RolledUpBalanceParams{TenantID: tid, ID: aid})
		return e
	})
	if err != nil {
		return domain.Money{}, fmt.Errorf("postgres: rolled up balance: %w", err)
	}
	return domain.NewMoney(amount, acct.Currency)
}

// AllAccountBalances returns every account for the tenant with its own
// derived balance and parent_id (ADR-023), for the caller to build the
// account tree and roll up balances in memory in one pass.
func (r *Repository) AllAccountBalances(ctx context.Context, tenantID string) ([]domain.AccountBalanceRow, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var rows []sqlc.AllAccountBalancesRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var e error
		rows, e = q.AllAccountBalances(ctx, tid)
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: all account balances: %w", err)
	}
	out := make([]domain.AccountBalanceRow, 0, len(rows))
	for _, row := range rows {
		acct, err := toDomainAccountFromBalanceRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.AccountBalanceRow{Account: acct, Balance: row.Balance})
	}
	return out, nil
}

// toDomainAccountFromBalanceRow builds a domain.Account from an
// AllAccountBalancesRow, mirroring accountFromRow for the balance-carrying
// row type sqlc generated for the AllAccountBalances query (its column list,
// including the trailing Balance, does not match GetAccountRow/ListAccountsRow,
// so it is its own sqlc struct).
func toDomainAccountFromBalanceRow(row sqlc.AllAccountBalancesRow) (domain.Account, error) {
	return accountFromRow(row.ID, row.Name, row.Type, row.Currency, row.Status, row.MinBalance, row.IsSystem, row.PartyReference, row.PartyType, row.ParentID)
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
	var row sqlc.GetOrCreateClearingAccountRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetOrCreateClearingAccount(ctx, sqlc.GetOrCreateClearingAccountParams{
			ID:       id,
			TenantID: tid,
			Name:     clearingAccountName(currency),
			Type:     domain.Liability.String(),
			Currency: string(currency),
		})
		return err
	})
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: get or create clearing account: %w", err)
	}
	// GetOrCreateClearingAccountRow does not select parent_id (a system
	// account is never given a parent): pass the zero pgtype.UUID, which maps
	// to nil the same as a real NULL would.
	return accountFromRow(row.ID, row.Name, row.Type, row.Currency, row.Status, row.MinBalance, row.IsSystem, row.PartyReference, row.PartyType, pgtype.UUID{})
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

		// Set the RLS GUC (migration 0024, Task 5.4b, audit A3.5)
		// transaction-local (set_config's third argument, true) so it
		// disappears when this attempt commits or rolls back rather than
		// leaking onto whatever request next borrows this pooled
		// connection. Parameterized, never string-interpolated: tenantID
		// is untrusted input from the request. Every write this attempt's
		// fn performs now runs with app.tenant_id set to tenantID, so a
		// write that ever forgot its own tenant_id filter (an UPDATE or
		// DELETE missing a WHERE, not just a SELECT) still cannot touch
		// another tenant's row: the FORCE ROW LEVEL SECURITY policies
		// restrict both the read side (USING) and the write side (WITH
		// CHECK) to this one tenant for the rest of the transaction.
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return fmt.Errorf("postgres: set tenant guc: %w", err)
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
	// ReversesTransactionID (Task 4.2, audit A1.2) is nil for an ordinary
	// post; only BuildReversal ever sets it.
	if t.ReversesTransactionID != nil {
		reversesID, err := uuid.Parse(*t.ReversesTransactionID)
		if err != nil {
			return fmt.Errorf("postgres: parse reverses transaction id: %w", err)
		}
		params.ReversesTransactionID = pgtype.UUID{Bytes: reversesID, Valid: true}
	}
	// reference and effective_at (Task 4.3, audit A1.3) are both nil for a
	// caller that supplies neither; t.Validate (called before this, by
	// Repository.CreateTransaction) already rejected a present-but-empty or
	// over-length reference, so nothing left to check here.
	if t.Reference != nil {
		params.Reference = pgtype.Text{String: *t.Reference, Valid: true}
	}
	if t.EffectiveAt != nil {
		params.EffectiveAt = pgtype.Timestamptz{Time: *t.EffectiveAt, Valid: true}
	}
	createdAt, err := tr.q.CreateTransaction(ctx, params)
	if err != nil {
		if isUniqueViolation(err) {
			switch pgConstraint(err) {
			// transactions_one_reversal_idx (migration 0017): a second
			// reversal of the same original. Distinguished from an ordinary
			// id collision (transactions_pkey) so the service can catch it
			// specifically and read back the existing reversal instead of
			// treating it as ErrDuplicateTransaction.
			case "transactions_one_reversal_idx":
				return domain.ErrTransactionAlreadyReversed
			// transactions_tenant_reference_idx (migration 0018): a second
			// transaction reusing a reference already taken in this tenant.
			// Distinguished from both transactions_pkey and
			// transactions_one_reversal_idx above, and deliberately its own
			// domain error (not ErrDuplicateTransaction): a duplicate
			// reference is a different failure than an id collision.
			case "transactions_tenant_reference_idx":
				return domain.ErrDuplicateReference
			default:
				return domain.ErrDuplicateTransaction
			}
		}
		return fmt.Errorf("postgres: insert transaction: %w", err)
	}
	// EffectiveAt's read-time fallback to created_at (see
	// Repository.transactionFromRow) applies just as much to the object this
	// call just built as to one read back later: without resolving it here,
	// a caller that reads t.EffectiveAt straight off the value CreateTransaction
	// was handed (the common case, no round trip through GetTransaction) would
	// see nil for a caller that omitted it, while every later GetTransaction
	// or List call on the very same row would see the fallback. Resolving it
	// here, from the RETURNING created_at above, keeps both views consistent
	// without a second query.
	if t.EffectiveAt == nil {
		t.EffectiveAt = &createdAt
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

// InsertIdempotencyKey records the key inside the surrounding transaction,
// with expires_at stamped as the DATABASE SERVER's now() + ttl (see
// idempotency.sql's InsertIdempotencyKey query), never this process's clock
// (Task 4.5, audit A1.4). The underlying query is an upsert: a conflict
// against an EXPIRED existing row for (tenantID, key) is replaced in place
// (RETURNING yields the row), while a conflict against a still-LIVE row
// leaves it untouched and RETURNING yields nothing, which pgx surfaces as
// pgx.ErrNoRows here; that case is mapped to ErrDuplicateIdempotencyKey so
// the service replays the original response exactly as it would for a plain
// unique-violation. scheme is stored alongside fingerprint (see
// domain.CurrentFingerprintScheme) so a future fingerprint-scheme change can
// recompute this row's fingerprint under the scheme that produced it instead
// of the scheme current at replay time.
func (tr txRepo) InsertIdempotencyKey(ctx context.Context, tenantID, key, fingerprint, scheme, transactionID string, ttl time.Duration) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	txID, err := uuid.Parse(transactionID)
	if err != nil {
		return fmt.Errorf("postgres: parse transaction id: %w", err)
	}
	if _, err := tr.q.InsertIdempotencyKey(ctx, sqlc.InsertIdempotencyKeyParams{
		TenantID:          tid,
		IdempotencyKey:    key,
		Fingerprint:       fingerprint,
		FingerprintScheme: scheme,
		TransactionID:     txID,
		TtlSeconds:        ttl.Seconds(),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
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
// e.TransactionID is nullable (ADR-025, migration 0034): empty means a
// non-transaction lifecycle event, mapped to a NULL column via
// optUUIDFromString, the same as e.SubjectID. e.HashVersion defaults to
// domain.AuditHashV1 when the caller left it zero, so every existing caller
// that predates ADR-025 (every transaction post) keeps writing v1 rows
// unchanged.
//
// The single background chainer (internal/audit.Chainer) is what later reads
// this row back and extends the tenant's tamper-evident hash chain.
func (tr txRepo) AppendAuditOutbox(ctx context.Context, tenantID string, e domain.AuditEvent) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	txID, err := optUUIDFromString(e.TransactionID)
	if err != nil {
		return fmt.Errorf("postgres: parse audit transaction id: %w", err)
	}
	subjectID, err := optUUIDFromString(e.SubjectID)
	if err != nil {
		return fmt.Errorf("postgres: parse audit subject id: %w", err)
	}
	hv := e.HashVersion
	if hv == 0 {
		hv = domain.AuditHashV1
	}
	if err := tr.q.InsertAuditOutbox(ctx, sqlc.InsertAuditOutboxParams{
		TenantID:      tid,
		Action:        e.Action,
		TransactionID: txID,
		Actor:         e.Actor,
		Before:        e.Before,
		After:         e.After,
		SubjectType:   optTextFromString(e.SubjectType),
		SubjectID:     subjectID,
		HashVersion:   int16(hv), //nolint:gosec // hv is one of the two small domain.AuditHashV* constants
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

// AccountPostingStates returns each of accountIDs's current status,
// min_balance, is_system flag, and derived balance, within the surrounding
// transaction (Task 5.5, audit A1.5). See domain.Tx.AccountPostingStates for
// the race-safety this depends on: read under the same SERIALIZABLE
// transaction CreateTransaction writes into right after, the same pattern
// TenantDailyDebits above already uses for the daily-volume policy check. An
// empty accountIDs returns an empty map without a round trip: ANY($2::uuid[])
// against an empty slice is well-defined SQL (it matches nothing), but a
// transaction can never touch zero accounts (Transaction.Validate requires
// at least two postings), so this is defense against a caller bug, not a
// real code path.
func (tr txRepo) AccountPostingStates(ctx context.Context, tenantID string, accountIDs []string) (map[string]domain.AccountPostingState, error) {
	if len(accountIDs) == 0 {
		return map[string]domain.AccountPostingState{}, nil
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	ids := make([]uuid.UUID, len(accountIDs))
	for i, id := range accountIDs {
		aid, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("postgres: parse account id: %w", err)
		}
		ids[i] = aid
	}

	// Phase 1: status, min_balance, is_system for every touched account.
	// This is the ONLY query that runs unconditionally on every post; see
	// AccountStatusFlags's own doc comment for why it is kept to the
	// accounts table alone.
	flagRows, err := tr.q.AccountStatusFlags(ctx, sqlc.AccountStatusFlagsParams{TenantID: tid, AccountIds: ids})
	if err != nil {
		return nil, fmt.Errorf("postgres: account status flags: %w", err)
	}
	out := make(map[string]domain.AccountPostingState, len(flagRows))
	var needBalance []uuid.UUID
	for _, row := range flagRows {
		state := domain.AccountPostingState{
			AccountID: row.ID.String(),
			Status:    domain.AccountStatus(row.Status),
			IsSystem:  row.IsSystem,
		}
		if row.MinBalance.Valid {
			v := row.MinBalance.Int64
			state.MinBalance = &v
			// A system account is exempt from the min_balance check
			// (domain.CheckAccountPostingConstraints), so its balance is
			// never inspected: skip the second, postings-touching query for
			// it even if a min_balance somehow ended up set on its row (the
			// public API never lets a caller set one, but this keeps the
			// exemption unconditional rather than "true only because nobody
			// configures this").
			if !row.IsSystem {
				needBalance = append(needBalance, row.ID)
			}
		}
		out[state.AccountID] = state
	}

	// Phase 2: derived balance, ONLY for accounts that actually have a
	// MinBalance configured (see AccountBalances's own doc comment for why
	// this read is not run unconditionally like phase 1 is).
	if len(needBalance) > 0 {
		balRows, err := tr.q.AccountBalances(ctx, sqlc.AccountBalancesParams{TenantID: tid, AccountIds: needBalance})
		if err != nil {
			return nil, fmt.Errorf("postgres: account balances: %w", err)
		}
		for _, row := range balRows {
			id := row.ID.String()
			state := out[id]
			state.Balance = row.Balance
			out[id] = state
		}
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
	var out domain.Transaction
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		row, err := q.GetTransaction(ctx, sqlc.GetTransactionParams{TenantID: tid, ID: txID})
		if err != nil {
			return err
		}
		out, err = transactionFromRow(ctx, q, tid, row)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Transaction{}, domain.ErrTransactionNotFound
	}
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("postgres: get transaction: %w", err)
	}
	return out, nil
}

// GetReversalOf returns the transaction that reverses originalID within
// tenantID, or domain.ErrTransactionNotFound if none exists yet (Task 4.2,
// audit A1.2). transactions_one_reversal_idx (migration 0017) guarantees at
// most one row can ever match.
func (r *Repository) GetReversalOf(ctx context.Context, tenantID, originalID string) (domain.Transaction, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	origID, err := uuid.Parse(originalID)
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("postgres: parse original transaction id: %w", err)
	}
	var out domain.Transaction
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		row, err := q.GetReversalOf(ctx, sqlc.GetReversalOfParams{
			TenantID:              tid,
			ReversesTransactionID: pgtype.UUID{Bytes: origID, Valid: true},
		})
		if err != nil {
			return err
		}
		out, err = transactionFromRow(ctx, q, tid, row)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Transaction{}, domain.ErrTransactionNotFound
	}
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("postgres: get reversal: %w", err)
	}
	return out, nil
}

// transactionFromRow loads tid's postings and assembles the full
// domain.Transaction from a sqlc.Transaction row already fetched by
// GetTransaction or GetReversalOf: both queries select the identical column
// set, so the postings fetch and the rest of the assembly (shared with
// ListTransactions, see assembleTransaction) are not duplicated per query.
// It takes q rather than closing over a Repository so the caller controls
// which transaction (and therefore which RLS GUC scope, see withTenant)
// the postings fetch runs in, the same one the row above was fetched in.
func transactionFromRow(ctx context.Context, q *sqlc.Queries, tid uuid.UUID, row sqlc.Transaction) (domain.Transaction, error) {
	rows, err := q.ListPostingsByTransaction(ctx, sqlc.ListPostingsByTransactionParams{
		TenantID:      tid,
		TransactionID: row.ID,
	})
	if err != nil {
		return domain.Transaction{}, fmt.Errorf("postgres: list postings: %w", err)
	}
	postings := make([]domain.Posting, 0, len(rows))
	for _, p := range rows {
		posting, err := postingFromRow(p.ID, p.AccountID, p.Amount, p.Currency, p.Description)
		if err != nil {
			return domain.Transaction{}, err
		}
		postings = append(postings, posting)
	}
	return assembleTransaction(row, postings), nil
}

// postingFromRow builds a domain.Posting from a posting row's columns,
// shared by transactionFromRow (one transaction's postings) and
// ListTransactions (a batch of many transactions' postings), so the
// currency-to-Money conversion is not duplicated per call site. Each posting
// carries its own currency (ADR-014): an FX transaction has two currencies in
// play, so Money is rebuilt per row, never from one transaction-wide
// currency.
func postingFromRow(id, accountID uuid.UUID, amount int64, currency, description string) (domain.Posting, error) {
	money, err := domain.NewMoney(amount, domain.Currency(currency))
	if err != nil {
		return domain.Posting{}, fmt.Errorf("postgres: build posting money: %w", err)
	}
	return domain.Posting{
		ID:          id.String(),
		AccountID:   accountID.String(),
		Amount:      money,
		Description: description,
	}, nil
}

// assembleTransaction builds a domain.Transaction from a sqlc.Transaction row
// and its already-loaded postings, shared by transactionFromRow (a single
// transaction) and ListTransactions (a page of many), so the FX snapshot,
// ReversesTransactionID, reference, and effective_at assembly is not
// duplicated per call site.
func assembleTransaction(row sqlc.Transaction, postings []domain.Posting) domain.Transaction {
	out := domain.Transaction{ID: row.ID.String(), Postings: postings}
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
	// reverses_transaction_id (Task 4.2, audit A1.2) is NULL for an ordinary
	// transaction; only a reversal carries it.
	if row.ReversesTransactionID.Valid {
		reversesID := uuid.UUID(row.ReversesTransactionID.Bytes).String()
		out.ReversesTransactionID = &reversesID
	}
	// reference (Task 4.3, audit A1.3) is NULL when the caller supplied
	// none; left nil rather than a pointer to "".
	if row.Reference.Valid {
		reference := row.Reference.String
		out.Reference = &reference
	}
	// effective_at falls back to created_at when NULL (Task 4.3, audit
	// A1.3): the column is never backfilled, so a transaction posted with no
	// value date reads back as having happened when its row was written.
	effectiveAt := row.CreatedAt
	if row.EffectiveAt.Valid {
		effectiveAt = row.EffectiveAt.Time
	}
	out.EffectiveAt = &effectiveAt
	return out
}

// ListTransactions returns up to limit of tenantID's transactions matching
// filter, newest first, keyset paged by (created_at, id) descending, the
// same cursor shape Statement uses (Task 4.4, audit A7.2). after is the
// keyset position to page from; nil starts at the newest transaction.
//
// Postings for the whole returned page are fetched in one extra batched round
// trip (ListPostingsByTransactionIDs) rather than one query per transaction,
// so this stays O(1) queries regardless of how many transactions the page
// contains, not O(n).
func (r *Repository) ListTransactions(ctx context.Context, tenantID string, filter domain.TransactionFilter, after *domain.StatementCursor, limit int) ([]domain.TransactionListItem, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}

	// First page: a sentinel that is strictly greater than any real
	// (created_at, id). Subsequent pages: the cursor handed back from the
	// previous page.
	afterTime, afterID := statementFirstPageTime, uuid.Max
	if after != nil {
		afterTime = after.CreatedAt
		if afterID, err = uuid.Parse(after.ID); err != nil {
			return nil, fmt.Errorf("postgres: parse cursor id: %w", err)
		}
	}

	var reference pgtype.Text
	if filter.Reference != nil {
		reference = pgtype.Text{String: *filter.Reference, Valid: true}
	}

	var rows []sqlc.Transaction
	var postingRows []sqlc.ListPostingsByTransactionIDsRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListTransactions(ctx, sqlc.ListTransactionsParams{
			TenantID:       tid,
			FromTs:         ptrToTimestamptz(filter.From),
			ToTs:           ptrToTimestamptz(filter.To),
			EffectiveFrom:  ptrToTimestamptz(filter.EffectiveFrom),
			EffectiveTo:    ptrToTimestamptz(filter.EffectiveTo),
			Reference:      reference,
			AfterCreatedAt: afterTime,
			AfterID:        afterID,
			PageLimit:      int32(limit), //nolint:gosec // limit is bounded by the API layer
		})
		if err != nil || len(rows) == 0 {
			return err
		}

		ids := make([]uuid.UUID, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
		}
		postingRows, err = q.ListPostingsByTransactionIDs(ctx, sqlc.ListPostingsByTransactionIDsParams{
			TenantID:       tid,
			TransactionIds: ids,
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list transactions: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	byTransaction := make(map[uuid.UUID][]domain.Posting, len(rows))
	for _, p := range postingRows {
		posting, err := postingFromRow(p.ID, p.AccountID, p.Amount, p.Currency, p.Description)
		if err != nil {
			return nil, err
		}
		byTransaction[p.TransactionID] = append(byTransaction[p.TransactionID], posting)
	}

	items := make([]domain.TransactionListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, domain.TransactionListItem{
			Transaction: assembleTransaction(row, byTransaction[row.ID]),
			CreatedAt:   row.CreatedAt,
		})
	}
	return items, nil
}

// GetIdempotencyKey returns the stored record for (tenantID, key), or
// domain.ErrIdempotencyKeyNotFound if none exists.
func (r *Repository) GetIdempotencyKey(ctx context.Context, tenantID, key string) (domain.IdempotencyRecord, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.IdempotencyRecord{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var row sqlc.GetIdempotencyKeyRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetIdempotencyKey(ctx, sqlc.GetIdempotencyKeyParams{TenantID: tid, IdempotencyKey: key})
		return err
	})
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
	var rows []sqlc.ListAuditByTransactionRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListAuditByTransaction(ctx, sqlc.ListAuditByTransactionParams{
			TenantID:      tid,
			TransactionID: pgtype.UUID{Bytes: txID, Valid: true},
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit by transaction: %w", err)
	}
	out := make([]domain.AuditEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.AuditEntry{
			ID:            row.ID.String(),
			Action:        row.Action,
			TransactionID: stringFromUUID(row.TransactionID),
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

	var rows []sqlc.ListAuditByAccountRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListAuditByAccount(ctx, sqlc.ListAuditByAccountParams{
			TenantID:  tid,
			AccountID: aid,
			AfterID:   afterID,
			PageLimit: int32(limit), //nolint:gosec // limit is bounded by the API layer
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit by account: %w", err)
	}
	out := make([]domain.AuditEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.AuditEntry{
			ID:            row.ID.String(),
			Action:        row.Action,
			TransactionID: stringFromUUID(row.TransactionID),
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

// ListAudit returns up to limit of the tenant's audit rows, newest first,
// keyset-paged by id (see ListAuditByAccount for why id drives ordering). The
// whole-tenant view backing GET /v1/audit.
func (r *Repository) ListAudit(ctx context.Context, tenantID string, after *domain.StatementCursor, limit int) ([]domain.AuditEntry, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	afterID := uuid.Max
	if after != nil {
		if afterID, err = uuid.Parse(after.ID); err != nil {
			return nil, fmt.Errorf("postgres: parse cursor id: %w", err)
		}
	}
	var rows []sqlc.ListAuditRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListAudit(ctx, sqlc.ListAuditParams{
			TenantID:  tid,
			AfterID:   afterID,
			PageLimit: int32(limit), //nolint:gosec // limit is bounded by the API layer
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit: %w", err)
	}
	out := make([]domain.AuditEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.AuditEntry{
			ID:            row.ID.String(),
			Action:        row.Action,
			TransactionID: stringFromUUID(row.TransactionID),
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
	var rows []sqlc.ListAuditForVerifyRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListAuditForVerify(ctx, tid)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit for verify: %w", err)
	}
	out := make([]domain.AuditEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.AuditEntry{
			ID:            row.ID.String(),
			Action:        row.Action,
			TransactionID: stringFromUUID(row.TransactionID),
			Actor:         row.Actor,
			Before:        row.Before,
			After:         row.After,
			CreatedAt:     row.CreatedAt,
			PrevHash:      row.PrevHash.String,
			RowHash:       row.RowHash.String,
			SubjectType:   row.SubjectType.String,
			SubjectID:     stringFromUUID(row.SubjectID),
			HashVersion:   int(row.HashVersion),
		})
	}
	return out, nil
}

// ListAuditForVerifyPage returns up to limit of the tenant's audit rows with
// ChainSeq strictly greater than afterChainSeq, in chain order, including
// ChainSeq (Task 5.3, audit A2.4): the bounded-memory counterpart to
// ListAuditForVerify, which AuditService.Verify calls in a loop instead of
// loading the whole chain at once. Runs through withTenant, exactly like
// every other tenant-scoped audit read here: paging must keep working with
// migration 0024's RLS in force for the tenant being verified (Task 5.4b).
func (r *Repository) ListAuditForVerifyPage(ctx context.Context, tenantID string, afterChainSeq int64, limit int) ([]domain.AuditEntry, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var rows []sqlc.ListAuditForVerifyPageRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListAuditForVerifyPage(ctx, sqlc.ListAuditForVerifyPageParams{
			TenantID:      tid,
			AfterChainSeq: afterChainSeq,
			PageLimit:     int32(limit), //nolint:gosec // limit is an application-configured page size, not user input
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit for verify page: %w", err)
	}
	out := make([]domain.AuditEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.AuditEntry{
			ID:            row.ID.String(),
			Action:        row.Action,
			TransactionID: stringFromUUID(row.TransactionID),
			Actor:         row.Actor,
			Before:        row.Before,
			After:         row.After,
			CreatedAt:     row.CreatedAt,
			PrevHash:      row.PrevHash.String,
			RowHash:       row.RowHash.String,
			ChainSeq:      row.ChainSeq,
			SubjectType:   row.SubjectType.String,
			SubjectID:     stringFromUUID(row.SubjectID),
			HashVersion:   int(row.HashVersion),
		})
	}
	return out, nil
}

// GetAuditHead returns the tenant's current chain head: the chain_seq and
// row_hash of its latest audit_log row (Task 5.3). ok is false when the
// tenant has no audit rows yet (pgx.ErrNoRows), not an error: an empty chain
// simply has no head to report.
func (r *Repository) GetAuditHead(ctx context.Context, tenantID string) (chainSeq int64, rowHash string, ok bool, err error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, "", false, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var row sqlc.GetAuditHeadRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetAuditHead(ctx, tid)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("postgres: get audit head: %w", err)
	}
	return row.ChainSeq, row.RowHash.String, true, nil
}

// LatestAuditAnchor returns the tenant's most recently recorded off-box
// anchor (Task 5.3, migration 0025). ok is false when no anchor has ever
// been recorded for this tenant (pgx.ErrNoRows), not an error.
func (r *Repository) LatestAuditAnchor(ctx context.Context, tenantID string) (domain.AuditAnchor, bool, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.AuditAnchor{}, false, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var row sqlc.GetLatestAuditAnchorRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetLatestAuditAnchor(ctx, tid)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AuditAnchor{}, false, nil
	}
	if err != nil {
		return domain.AuditAnchor{}, false, fmt.Errorf("postgres: get latest audit anchor: %w", err)
	}
	return domain.AuditAnchor{ChainSeq: row.ChainSeq, RowHash: row.RowHash, CreatedAt: row.CreatedAt}, true, nil
}

// CountPendingOutbox returns the number of tenantID's audit_outbox rows the
// chainer has not yet processed (ADR-017): the audit chain's lag, surfaced by
// audit verify alongside the chained head.
func (r *Repository) CountPendingOutbox(ctx context.Context, tenantID string) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var n int64
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		n, err = q.CountPendingOutbox(ctx, tid)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: count pending outbox: %w", err)
	}
	return int(n), nil
}

// sweepBatchSize bounds each delete statement the idempotency sweep issues
// (see SweepExpiredIdempotencyKeys) so a large backlog of expired keys is
// reclaimed in bounded chunks instead of one unbounded DELETE contending
// with live posts. A constant rather than an env knob: there is no
// deployment-shaped reason to tune it, just a sane default.
const sweepBatchSize = 1000

// sweepMaxIterations bounds how many batches a single
// SweepExpiredIdempotencyKeys call will issue, so a pathological backlog (or
// a backlog that keeps growing as fast as it drains) can never turn one
// sweep tick into an unbounded loop; the remainder is picked up on the next
// scheduled tick.
const sweepMaxIterations = 1000

// SweepExpiredIdempotencyKeys deletes every idempotency_keys row whose
// expires_at has passed, across every tenant, and returns how many rows it
// deleted in total (Task 4.5, audit A1.4). It runs directly against the
// pool, not inside RunInTx: it is a plain maintenance statement, never part
// of a request's unit of work.
//
// It deletes in bounded batches of sweepBatchSize rows (a single unbounded
// DELETE could lock and remove an arbitrarily large number of rows in one
// statement, contending with live posts under a large backlog) rather than
// one statement for the whole table. It loops the batched delete until a
// batch reports 0 rows deleted, up to sweepMaxIterations batches as a safety
// valve, respecting context cancellation between batches. It is best-effort
// maintenance: an error from any batch is returned (and logged by the
// caller) without panicking, and rows already deleted by prior batches in
// this call stay deleted.
func (r *Repository) SweepExpiredIdempotencyKeys(ctx context.Context) (int64, error) {
	var total int64
	for range sweepMaxIterations {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("postgres: sweep expired idempotency keys: %w", err)
		}
		n, err := r.q.SweepExpiredIdempotencyKeysBatch(ctx, sweepBatchSize)
		if err != nil {
			return total, fmt.Errorf("postgres: sweep expired idempotency keys: %w", err)
		}
		total += n
		if n < sweepBatchSize {
			// Fewer rows than the batch size means this batch drained
			// everything currently expired; no need to issue another
			// statement that would just find 0 rows.
			break
		}
	}
	return total, nil
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
	var sum int64
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		sum, err = q.AccountBalance(ctx, sqlc.AccountBalanceParams{TenantID: tid, AccountID: aid})
		return err
	})
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

	var rows []sqlc.AccountStatementRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.AccountStatement(ctx, sqlc.AccountStatementParams{
			TenantID:       tid,
			AccountID:      aid,
			AfterCreatedAt: afterTime,
			AfterID:        afterID,
			PageLimit:      int32(limit), //nolint:gosec // limit is bounded by the API layer
		})
		return err
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

// StatementExport returns up to limit postings affecting the account within
// an optional [from, to) created_at window, newest first, each with its
// running balance (Task 6.3, audit A9.2): the per-account period statement
// export's bounded, unpaged counterpart to Statement.
func (r *Repository) StatementExport(ctx context.Context, tenantID, accountID string, currency domain.Currency, from, to *time.Time, limit int) ([]domain.StatementEntry, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	aid, err := uuid.Parse(accountID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse account id: %w", err)
	}

	var rows []sqlc.AccountStatementRangeRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.AccountStatementRange(ctx, sqlc.AccountStatementRangeParams{
			TenantID:  tid,
			AccountID: aid,
			FromTs:    ptrToTimestamptz(from),
			ToTs:      ptrToTimestamptz(to),
			RowLimit:  int32(limit), //nolint:gosec // limit is bounded by the API layer
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: account statement export: %w", err)
	}

	entries := make([]domain.StatementEntry, 0, len(rows))
	for _, row := range rows {
		amount, err := domain.NewMoney(row.Amount, currency)
		if err != nil {
			return nil, fmt.Errorf("postgres: statement export amount: %w", err)
		}
		running, err := domain.NewMoney(row.RunningBalance, currency)
		if err != nil {
			return nil, fmt.Errorf("postgres: statement export running balance: %w", err)
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

// SetAPIKeyScopesByHash overwrites the scopes of the api_keys row matching
// keyHash to exactly scopes (ADR-019 follow-up): unlike InsertAPIKey, which
// is insert-or-ignore on the unique key_hash, this always applies, so it is
// how a caller reconciles an already-existing key's scopes (for example the
// demo key's, on every boot) without needing that row's id. A hash with no
// matching row affects zero rows and returns nil.
func (r *Repository) SetAPIKeyScopesByHash(ctx context.Context, keyHash string, scopes []domain.Scope) error {
	if err := r.q.SetAPIKeyScopesByHash(ctx, sqlc.SetAPIKeyScopesByHashParams{
		KeyHash: keyHash,
		Scopes:  scopesToStrings(scopes),
	}); err != nil {
		return fmt.Errorf("postgres: set api key scopes by hash: %w", err)
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

	params := sqlc.InsertFXRateParams{
		TenantID:  pgTenantID,
		Base:      string(base),
		Quote:     string(quote),
		MidRateE8: midRateE8,
		// This entry point always takes an explicit spread (validated above,
		// never NULL): ADR-020's nullable spread_bps is for a row that
		// deliberately falls back to the markup default, which this signature
		// has no way to express. Valid: true marks it a per-pair override.
		SpreadBps:   pgtype.Int4{Int32: spreadBps, Valid: true},
		Source:      source,
		EffectiveAt: pgEffectiveAt,
	}
	// A tenant-specific rate is inserted with the GUC set to that tenant
	// (fx_rates's RLS policy, migration 0024, still allows it: WITH CHECK
	// is "tenant_id IS NULL OR <matches the GUC>", and this row's tenant_id
	// equals the GUC). A global rate (tenantID nil, tenant_id column left
	// NULL) has no tenant to scope the GUC to; its WITH CHECK passes
	// unconditionally via the "tenant_id IS NULL" branch regardless, so it
	// is inserted directly on the pool, same as before this migration.
	if tenantID != nil {
		err := r.withTenant(ctx, *tenantID, func(q *sqlc.Queries) error {
			_, err := q.InsertFXRate(ctx, params)
			return err
		})
		if err != nil {
			return fmt.Errorf("postgres: insert fx rate: %w", err)
		}
		return nil
	}
	if _, err := r.q.InsertFXRate(ctx, params); err != nil {
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

// ptrToInt8 converts *int64 to a nullable Postgres int8, NULL when p is nil.
// Used for accounts.min_balance (Task 5.5, audit A1.5): nil means "no floor
// configured", the same meaning NULL carries in the column.
func ptrToInt8(p *int64) pgtype.Int8 {
	if p == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *p, Valid: true}
}

// ptrToText converts *string to a nullable Postgres text, NULL when p is
// nil. Used for accounts.party_reference and accounts.party_type (Task 6.1,
// audit A9.1): nil means "no party linkage supplied", the same meaning NULL
// carries in the column.
func ptrToText(p *string) pgtype.Text {
	if p == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *p, Valid: true}
}

// textToPtr is the inverse of ptrToText: nil when t is not Valid (NULL in
// the column), otherwise a pointer to its string value.
func textToPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	v := t.String
	return &v
}

// optUUID maps an optional string id to a pgtype.UUID (Valid false when nil
// or empty). Used for accounts.parent_id (ADR-023): nil means "no parent
// (a root account)", the same meaning NULL carries in the column.
func optUUID(id *string) (pgtype.UUID, error) {
	if id == nil || *id == "" {
		return pgtype.UUID{}, nil
	}
	u, err := uuid.Parse(*id)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("postgres: parse uuid %q: %w", *id, err)
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

// uuidToPtr is the inverse of optUUID: nil when u is not Valid (NULL in the
// column), otherwise a pointer to its string form. Used for accounts.parent_id
// (ADR-023), the same convention textToPtr follows for a nullable text column.
func uuidToPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

// optTextFromString maps a possibly-empty string to a pgtype.Text (Valid
// false when empty), the same empty-string-means-NULL convention
// optUUIDFromString follows for a nullable uuid column. Used for
// domain.AuditEvent/AuditEntry's SubjectType (ADR-025, migration 0034).
func optTextFromString(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// optUUIDFromString maps a possibly-empty string id to a pgtype.UUID (Valid
// false when the string is empty), the same nullable convention optUUID
// follows for a *string. Used for domain.AuditEvent/AuditEntry's
// TransactionID and SubjectID (ADR-025, migration 0034): both are plain
// strings where "" means "none" (a NULL column), never a pointer, since
// AuditEvent/AuditEntry predate this nullable case and every other caller
// still always supplies a real transaction id.
func optUUIDFromString(id string) (pgtype.UUID, error) {
	if id == "" {
		return pgtype.UUID{}, nil
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("postgres: parse uuid %q: %w", id, err)
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

// stringFromUUID is the inverse of optUUIDFromString: "" when u is not Valid
// (NULL in the column), otherwise its string form. Used to map a nullable
// transaction_id/subject_id column back onto AuditEvent/AuditEntry's plain
// string fields.
func stringFromUUID(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// mapHierarchyErr turns the hierarchy guard's check_violation (23514) into
// domain.ErrInvalidHierarchy and the parent FK's foreign_key_violation
// (23503) into domain.ErrParentNotFound (ADR-023), so the API returns 422
// rather than 500.
func mapHierarchyErr(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23514":
			return fmt.Errorf("%w: %s", domain.ErrInvalidHierarchy, pgErr.Message)
		case "23503":
			if pgErr.ConstraintName == "accounts_parent_fk" {
				return domain.ErrParentNotFound
			}
		}
	}
	return err
}

// accountFromRow builds a domain.Account from the scalar fields common to every
// account-shaped row sqlc generates (GetAccountRow, ListAccountsRow, ...). Taking
// individual fields rather than one sqlc row type is deliberate: sqlc gives each
// query its own row struct whenever its column list doesn't exactly match every
// column of the accounts table (which stopped being true once the schema grew
// columns like is_system that not every query selects), so a single shared
// struct type would not compile across call sites.
//
// status, minBalance, and isSystem are Task 5.5 (audit A1.5) additions;
// partyReference and partyType are Task 6.1 (audit A9.1) additions;
// parentID is an ADR-023 addition: every query that selects them (GetAccount,
// ListAccounts, GetOrCreateClearingAccount) passes its own row's values
// through unchanged. GetOrCreateClearingAccount's row never selects
// parent_id (a system account is never given a parent), so it passes the
// zero pgtype.UUID, which maps to nil the same as a real NULL would.
func accountFromRow(id uuid.UUID, name, accountType, currency, status string, minBalance pgtype.Int8, isSystem bool, partyReference, partyType pgtype.Text, parentID pgtype.UUID) (domain.Account, error) {
	at, err := domain.ParseAccountType(accountType)
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: parse account type: %w", err)
	}
	a := domain.Account{
		ID:             id.String(),
		Name:           name,
		Type:           at,
		Currency:       domain.Currency(currency),
		Status:         domain.AccountStatus(status),
		System:         isSystem,
		PartyReference: textToPtr(partyReference),
		PartyType:      textToPtr(partyType),
		ParentID:       uuidToPtr(parentID),
	}
	if minBalance.Valid {
		v := minBalance.Int64
		a.MinBalance = &v
	}
	return a, nil
}

// CreateWebhookSubscription assigns an identity if sub.ID is empty and
// inserts sub with secret as its stored HMAC signing key (Task 4.1, audit
// A7.1). It precisely mirrors InsertFXRate's own tenant-existence precheck
// (a plain GetTenant lookup before ever writing) rather than catching the
// webhook_subscriptions_tenant_id_fkey violation after the fact, so a
// missing tenant surfaces as domain.ErrTenantNotFound instead of a raw
// foreign-key-violation error reaching the caller.
func (r *Repository) CreateWebhookSubscription(ctx context.Context, sub *domain.WebhookSubscription, secret string) error {
	if sub.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("postgres: generate webhook subscription id: %w", err)
		}
		sub.ID = id.String()
	}
	id, err := uuid.Parse(sub.ID)
	if err != nil {
		return fmt.Errorf("postgres: parse webhook subscription id: %w", err)
	}
	tid, err := uuid.Parse(sub.TenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if _, err := r.q.GetTenant(ctx, tid); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrTenantNotFound
		}
		return fmt.Errorf("postgres: check tenant exists: %w", err)
	}

	eventTypes := sub.EventTypes
	if eventTypes == nil {
		eventTypes = []string{}
	}
	err = r.withTenant(ctx, sub.TenantID, func(q *sqlc.Queries) error {
		return q.InsertWebhookSubscription(ctx, sqlc.InsertWebhookSubscriptionParams{
			ID:         id,
			TenantID:   tid,
			Url:        sub.URL,
			Secret:     secret,
			EventTypes: eventTypes,
		})
	})
	if err != nil {
		return fmt.Errorf("postgres: insert webhook subscription: %w", err)
	}
	sub.Active = true
	return nil
}

// ListWebhookSubscriptionsByTenant returns every webhook_subscriptions row
// for tenantID, oldest first, active or not (Task 4.1). Never carries a
// secret: domain.WebhookSubscription has no field for one.
func (r *Repository) ListWebhookSubscriptionsByTenant(ctx context.Context, tenantID string) ([]domain.WebhookSubscription, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var rows []sqlc.ListWebhookSubscriptionsByTenantRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListWebhookSubscriptionsByTenant(ctx, tid)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list webhook subscriptions by tenant: %w", err)
	}
	out := make([]domain.WebhookSubscription, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.WebhookSubscription{
			ID:         row.ID.String(),
			TenantID:   row.TenantID.String(),
			URL:        row.Url,
			EventTypes: row.EventTypes,
			Active:     row.Active,
			CreatedAt:  row.CreatedAt,
		})
	}
	return out, nil
}

// SetWebhookSubscriptionActive sets active for the subscription identified
// by id (Task 4.1), or returns domain.ErrWebhookSubscriptionNotFound if no
// subscription matches id.
func (r *Repository) SetWebhookSubscriptionActive(ctx context.Context, id string, active bool) error {
	subID, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("postgres: parse webhook subscription id: %w", err)
	}
	rows, err := r.q.SetWebhookSubscriptionActive(ctx, sqlc.SetWebhookSubscriptionActiveParams{
		Active: active,
		ID:     subID,
	})
	if err != nil {
		return fmt.Errorf("postgres: set webhook subscription active: %w", err)
	}
	if rows == 0 {
		return domain.ErrWebhookSubscriptionNotFound
	}
	return nil
}

// pendingFromRow builds a domain.PendingTransaction from a sqlc.PendingTransaction
// row, shared by GetPendingTransaction, GetPendingForUpdate,
// ListPendingTransactions, and SweepExpiredPending (Task 4, ADR-025): every
// one of those selects the identical column set.
func pendingFromRow(row sqlc.PendingTransaction) domain.PendingTransaction {
	p := domain.PendingTransaction{
		ID:            row.ID.String(),
		TenantID:      row.TenantID.String(),
		Kind:          domain.PendingKind(row.Kind),
		Payload:       json.RawMessage(row.Payload),
		Status:        domain.PendingStatus(row.Status),
		ThresholdCcy:  row.ThresholdCcy,
		ThresholdAmt:  row.ThresholdAmt,
		CreatedBy:     row.CreatedBy,
		CreatedAt:     row.CreatedAt,
		DecidedBy:     textToPtr(row.DecidedBy),
		Reason:        textToPtr(row.Reason),
		TransactionID: uuidToPtr(row.TransactionID),
	}
	if row.DecidedAt.Valid {
		t := row.DecidedAt.Time
		p.DecidedAt = &t
	}
	return p
}

// InsertPendingTransaction assigns an identity if p.ID is empty, defaults
// Status to pending, and inserts p (Task 4, ADR-025): an over-threshold
// transaction held as intent. Nothing in postings/transactions is touched by
// this call. It returns domain.ErrInvalidPendingTransaction if p carries an
// unrecognized Kind, an empty Payload, or an empty ThresholdCcy/CreatedBy.
func (r *Repository) InsertPendingTransaction(ctx context.Context, tenantID string, p *domain.PendingTransaction) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if p.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("postgres: generate pending transaction id: %w", err)
		}
		p.ID = id.String()
	}
	if !p.Kind.Valid() || len(p.Payload) == 0 || p.ThresholdCcy == "" || p.CreatedBy == "" {
		return domain.ErrInvalidPendingTransaction
	}
	p.TenantID = tenantID
	p.Status = domain.PendingStatusPending
	pid, err := uuid.Parse(p.ID)
	if err != nil {
		return fmt.Errorf("postgres: parse pending transaction id: %w", err)
	}
	return r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		return q.InsertPendingTransaction(ctx, sqlc.InsertPendingTransactionParams{
			ID:           pid,
			TenantID:     tid,
			Kind:         string(p.Kind),
			Payload:      p.Payload,
			ThresholdCcy: p.ThresholdCcy,
			ThresholdAmt: p.ThresholdAmt,
			CreatedBy:    p.CreatedBy,
		})
	})
}

// GetPendingTransaction returns the pending transaction, or
// domain.ErrPendingTransactionNotFound if absent (Task 4, ADR-025).
func (r *Repository) GetPendingTransaction(ctx context.Context, tenantID, id string) (*domain.PendingTransaction, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	pid, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse pending transaction id: %w", err)
	}
	var row sqlc.PendingTransaction
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetPendingTransaction(ctx, sqlc.GetPendingTransactionParams{TenantID: tid, ID: pid})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPendingTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get pending transaction: %w", err)
	}
	p := pendingFromRow(row)
	return &p, nil
}

// ListPendingTransactions returns up to limit of the tenant's pending
// transactions, newest first, keyset paged, optionally filtered by status
// (Task 4, ADR-025).
func (r *Repository) ListPendingTransactions(ctx context.Context, tenantID string, status *domain.PendingStatus, after *domain.StatementCursor, limit int) ([]domain.PendingTransaction, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}

	afterTime, afterID := statementFirstPageTime, uuid.Max
	if after != nil {
		afterTime = after.CreatedAt
		if afterID, err = uuid.Parse(after.ID); err != nil {
			return nil, fmt.Errorf("postgres: parse cursor id: %w", err)
		}
	}

	var statusFilter pgtype.Text
	if status != nil {
		statusFilter = pgtype.Text{String: string(*status), Valid: true}
	}

	var rows []sqlc.PendingTransaction
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListPendingTransactions(ctx, sqlc.ListPendingTransactionsParams{
			TenantID:       tid,
			Status:         statusFilter,
			AfterCreatedAt: afterTime,
			AfterID:        afterID,
			PageLimit:      int32(limit), //nolint:gosec // limit is bounded by the API layer
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list pending transactions: %w", err)
	}
	out := make([]domain.PendingTransaction, 0, len(rows))
	for _, row := range rows {
		out = append(out, pendingFromRow(row))
	}
	return out, nil
}

// SweepExpiredPending moves every still-pending row older than olderThan to
// domain.PendingStatusExpired (decided_by "system"), across every tenant,
// and returns the rows it expired (Task 4, ADR-025). Not tenant-scoped and
// not run inside RunInTx, the same convention SweepExpiredIdempotencyKeys
// follows: it runs directly against r.q (the pool), with no app.tenant_id
// GUC ever set on that connection, so the RLS policy's allow-when-unset
// branch is what lets it see and update every tenant's rows.
func (r *Repository) SweepExpiredPending(ctx context.Context, olderThan time.Duration) ([]domain.PendingTransaction, error) {
	rows, err := r.q.SweepExpiredPending(ctx, olderThan.Seconds())
	if err != nil {
		return nil, fmt.Errorf("postgres: sweep expired pending: %w", err)
	}
	out := make([]domain.PendingTransaction, 0, len(rows))
	for _, row := range rows {
		out = append(out, pendingFromRow(row))
	}
	return out, nil
}

// PendingApprovedForTransaction reports whether an approved pending
// transaction produced txID (Task 6, ADR-025): the reverse-of-approved
// exemption's read, called by TransactionService.ReverseTransaction before
// gating a reversal, never from inside RunInTx.
func (r *Repository) PendingApprovedForTransaction(ctx context.Context, tenantID, txID string) (bool, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return false, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	xid, err := optUUID(&txID)
	if err != nil {
		return false, fmt.Errorf("postgres: parse transaction id: %w", err)
	}
	var exists bool
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		exists, err = q.PendingApprovedForTransaction(ctx, sqlc.PendingApprovedForTransactionParams{
			TenantID:      tid,
			TransactionID: xid,
		})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("postgres: pending approved for transaction: %w", err)
	}
	return exists, nil
}

// InsertPendingTransaction is the domain.Tx counterpart to
// Repository.InsertPendingTransaction (Task 6, ADR-025): the same insert,
// but run against the bound transaction's queries (tr.q) rather than a
// dedicated withTenant call, so holdForApproval can write the pending and
// its approval.requested audit_outbox row together inside one RunInTx.
func (tr txRepo) InsertPendingTransaction(ctx context.Context, tenantID string, p *domain.PendingTransaction) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if p.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("postgres: generate pending transaction id: %w", err)
		}
		p.ID = id.String()
	}
	if !p.Kind.Valid() || len(p.Payload) == 0 || p.ThresholdCcy == "" || p.CreatedBy == "" {
		return domain.ErrInvalidPendingTransaction
	}
	p.TenantID = tenantID
	p.Status = domain.PendingStatusPending
	pid, err := uuid.Parse(p.ID)
	if err != nil {
		return fmt.Errorf("postgres: parse pending transaction id: %w", err)
	}
	return tr.q.InsertPendingTransaction(ctx, sqlc.InsertPendingTransactionParams{
		ID:           pid,
		TenantID:     tid,
		Kind:         string(p.Kind),
		Payload:      p.Payload,
		ThresholdCcy: p.ThresholdCcy,
		ThresholdAmt: p.ThresholdAmt,
		CreatedBy:    p.CreatedBy,
	})
}

// GetPendingForUpdate returns the pending transaction, row-locked (SELECT
// ... FOR UPDATE) for the rest of the surrounding transaction, or
// domain.ErrPendingTransactionNotFound if absent (Task 4, ADR-025).
func (tr txRepo) GetPendingForUpdate(ctx context.Context, tenantID, id string) (*domain.PendingTransaction, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	pid, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse pending transaction id: %w", err)
	}
	row, err := tr.q.GetPendingForUpdate(ctx, sqlc.GetPendingForUpdateParams{TenantID: tid, ID: pid})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPendingTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get pending transaction for update: %w", err)
	}
	p := pendingFromRow(row)
	return &p, nil
}

// UpdatePendingStatus transitions the pending transaction to status,
// stamping decidedBy and the server clock's decided_at, and reason/txID if
// given (Task 4, ADR-025). Always called after GetPendingForUpdate has
// already locked the row within the same surrounding transaction: this is a
// plain unconditional update, not a guarded one the way
// Repository.ResolveDispute's UPDATE ... WHERE status = 'open' is, since
// the row lock, not a WHERE clause here, is what prevents a second
// concurrent decision.
func (tr txRepo) UpdatePendingStatus(ctx context.Context, tenantID string, id string, status domain.PendingStatus, decidedBy string, reason *string, txID *string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	pid, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("postgres: parse pending transaction id: %w", err)
	}
	txnID, err := optUUID(txID)
	if err != nil {
		return fmt.Errorf("postgres: parse pending decision transaction id: %w", err)
	}
	return tr.q.UpdatePendingStatus(ctx, sqlc.UpdatePendingStatusParams{
		Status:        string(status),
		DecidedBy:     pgtype.Text{String: decidedBy, Valid: true},
		Reason:        ptrToText(reason),
		TransactionID: txnID,
		TenantID:      tid,
		ID:            pid,
	})
}
