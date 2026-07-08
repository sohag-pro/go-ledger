package domain

import (
	"context"
	"time"
)

// StatementEntry is one line of an account statement: a posting that affected the
// account, with the account's running balance as of that posting. Amount and
// RunningBalance are in the account's currency.
type StatementEntry struct {
	// ID is the posting's own id. It is the keyset tiebreaker for paging and what
	// a statement cursor points at.
	ID             string
	TransactionID  string
	Amount         Money
	RunningBalance Money
	Description    string
	CreatedAt      time.Time
}

// StatementCursor is an opaque keyset position for paging a statement. It points
// at the last entry of the previous page; the next page returns entries strictly
// older than it, by (CreatedAt, ID) descending.
type StatementCursor struct {
	CreatedAt time.Time
	ID        string
}

// Tx is a unit of work bound to a single database transaction. The service
// layer composes one or more writes inside RunInTx; everything done through a Tx
// commits together or not at all.
type Tx interface {
	// CreateTransaction validates t and persists the transaction and all its
	// postings within the surrounding transaction. If t.ID is empty an id is
	// assigned and written back to t.
	CreateTransaction(ctx context.Context, tenantID string, t *Transaction) error

	// InsertIdempotencyKey records key with the request fingerprint and the
	// transaction it produced, within the surrounding transaction. It returns
	// ErrDuplicateIdempotencyKey if (tenantID, key) already exists.
	InsertIdempotencyKey(ctx context.Context, tenantID, key, fingerprint, transactionID string) error

	// AppendAudit writes one audit row within the surrounding transaction.
	AppendAudit(ctx context.Context, tenantID string, e AuditEntry) error
}

// Repository is the persistence port for the ledger. The domain owns this
// contract; storage adapters (see internal/postgres) implement it. Every method
// is scoped by tenantID: the ledger is multi-tenant, and a tenant can only ever
// see its own accounts, transactions, and balances.
//
// Writes follow the core invariant: postings are append-only and balances are
// derived, never stored. CreateTransaction validates the double-entry invariant
// before persisting and writes the transaction and all its postings atomically.
type Repository interface {
	// CreateAccount persists a. If a.ID is empty the adapter assigns one and
	// writes it back to a. It returns the account's validation error if a is
	// invalid.
	CreateAccount(ctx context.Context, tenantID string, a *Account) error

	// GetAccount returns the account with the given id within the tenant, or
	// ErrAccountNotFound if none exists.
	GetAccount(ctx context.Context, tenantID, id string) (Account, error)

	// ListAccounts returns up to limit of the tenant's accounts, ordered by name.
	// Accounts are a small bounded set, so this is a simple capped list rather
	// than a paginated cursor.
	ListAccounts(ctx context.Context, tenantID string, limit int) ([]Account, error)

	// CreateTransaction validates t (the double-entry invariant) and persists the
	// transaction together with all its postings in a single atomic database
	// transaction. If t.ID is empty the adapter assigns one and writes it back to
	// t; the same applies to each posting's identity. It returns t's validation
	// error if t is invalid.
	CreateTransaction(ctx context.Context, tenantID string, t *Transaction) error

	// GetTransaction returns the transaction with the given id within the tenant,
	// including all its postings, or ErrTransactionNotFound if none exists.
	GetTransaction(ctx context.Context, tenantID, id string) (Transaction, error)

	// GetIdempotencyKey returns the stored record for (tenantID, key), or
	// ErrIdempotencyKeyNotFound if none exists.
	GetIdempotencyKey(ctx context.Context, tenantID, key string) (IdempotencyRecord, error)

	// ListAuditByTransaction returns the audit rows for a transaction, oldest
	// first. An unknown transaction yields no rows.
	ListAuditByTransaction(ctx context.Context, tenantID, transactionID string) ([]AuditEntry, error)

	// ListAuditByAccount returns up to limit audit rows for every transaction
	// that has a posting touching the account, newest first (keyset paged). after
	// is the keyset position to page from; nil starts at the newest entry. An
	// unknown account yields no rows.
	ListAuditByAccount(ctx context.Context, tenantID, accountID string, after *StatementCursor, limit int) ([]AuditEntry, error)

	// ListAuditForVerify returns every audit row for the tenant, oldest first
	// (created_at, id ascending), including PrevHash and RowHash. It is the
	// full per-tenant walk used to recompute and check the tamper-evident hash
	// chain end to end, not a paged read for display.
	ListAuditForVerify(ctx context.Context, tenantID string) ([]AuditEntry, error)

	// Balance returns the derived balance of an account: the sum of its postings'
	// signed amounts. It returns ErrAccountNotFound if the account does not exist.
	//
	// Balance is a non-snapshot read: the existence check and the sum are not
	// guaranteed to observe the same instant, so a balance read concurrent with a
	// posting may reflect either side of that write. This is fine for an
	// eventually-summed balance; a caller that needs a point-in-time consistent
	// read should perform it inside RunInTx.
	Balance(ctx context.Context, tenantID, accountID string) (Money, error)

	// Statement returns up to limit postings affecting the account, newest first,
	// each carrying the account's running balance as of that posting. after is the
	// keyset position to page from; nil starts at the newest entry. currency is the
	// account's currency, used to build the Money values. The caller is expected to
	// have resolved the account (for its currency and existence) first; an unknown
	// account simply yields no entries.
	Statement(ctx context.Context, tenantID, accountID string, currency Currency, after *StatementCursor, limit int) ([]StatementEntry, error)

	// RunInTx executes fn inside a SERIALIZABLE database transaction, scoped to
	// tenantID. It commits if fn returns nil and rolls back otherwise. Because
	// SERIALIZABLE can abort a transaction with a serialization conflict under
	// concurrency, the adapter retries fn automatically a bounded number of
	// times; fn must therefore be safe to run more than once. It returns the
	// last error if retries are exhausted, or any non-retryable error from fn.
	//
	// tenantID also picks the per-tenant session advisory lock the adapter
	// acquires before opening the transaction: same-tenant calls serialize one
	// at a time, while different tenants run fully concurrently. This is what
	// keeps the per-tenant audit hash chain (ADR-012) from repeatedly aborting
	// concurrent same-tenant writers with a serialization failure (SQLSTATE
	// 40001); see the adapter's RunInTx for why the lock must be a session lock
	// taken before the transaction begins, not one taken inside it.
	RunInTx(ctx context.Context, tenantID string, fn func(context.Context, Tx) error) error

	// GetAPIKeyByHash resolves an unrevoked api_keys row by the SHA-256 hex hash
	// of a presented key, or ErrAPIKeyNotFound if no such unrevoked key exists.
	GetAPIKeyByHash(ctx context.Context, hash string) (APIKey, error)

	// InsertAPIKey persists k with keyHash as its stored credential. Only the
	// hash is ever written; the plaintext is never stored.
	InsertAPIKey(ctx context.Context, k APIKey, keyHash string) error
}
