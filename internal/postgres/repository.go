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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	return accountFromRow(row)
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
		acct, err := accountFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, acct)
	}
	return out, nil
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
	return r.RunInTx(ctx, func(ctx context.Context, tx domain.Tx) error {
		return tx.CreateTransaction(ctx, tenantID, t)
	})
}

// RunInTx executes fn inside a SERIALIZABLE transaction, committing on success
// and rolling back on error. SERIALIZABLE can abort a transaction with a
// serialization conflict (SQLSTATE 40001), and the conflict often surfaces only
// at COMMIT, so both fn and the commit are watched and the whole unit of work is
// replayed up to maxPostAttempts times. fn must therefore be safe to run more
// than once.
func (r *Repository) RunInTx(ctx context.Context, fn func(context.Context, domain.Tx) error) error {
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
	// A valid transaction has at least one posting in a single shared currency.
	currency := string(t.Postings[0].Amount.Currency())

	if err := tr.q.CreateTransaction(ctx, sqlc.CreateTransactionParams{
		ID:       txID,
		TenantID: tid,
		Currency: currency,
	}); err != nil {
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
			Description:   p.Description,
		}); err != nil {
			return fmt.Errorf("postgres: insert posting: %w", err)
		}
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
	currency := domain.Currency(row.Currency)
	out := domain.Transaction{ID: row.ID.String(), Postings: make([]domain.Posting, 0, len(postings))}
	for _, p := range postings {
		money, err := domain.NewMoney(p.Amount, currency)
		if err != nil {
			return domain.Transaction{}, fmt.Errorf("postgres: build posting money: %w", err)
		}
		out.Postings = append(out.Postings, domain.Posting{
			AccountID:   p.AccountID.String(),
			Amount:      money,
			Description: p.Description,
		})
	}
	return out, nil
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

func accountFromRow(row sqlc.Account) (domain.Account, error) {
	at, err := domain.ParseAccountType(row.Type)
	if err != nil {
		return domain.Account{}, fmt.Errorf("postgres: parse account type: %w", err)
	}
	return domain.Account{
		ID:       row.ID.String(),
		Name:     row.Name,
		Type:     at,
		Currency: domain.Currency(row.Currency),
	}, nil
}
