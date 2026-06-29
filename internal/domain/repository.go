package domain

import "context"

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

	// CreateTransaction validates t (the double-entry invariant) and persists the
	// transaction together with all its postings in a single atomic database
	// transaction. If t.ID is empty the adapter assigns one and writes it back to
	// t; the same applies to each posting's identity. It returns t's validation
	// error if t is invalid.
	CreateTransaction(ctx context.Context, tenantID string, t *Transaction) error

	// GetTransaction returns the transaction with the given id within the tenant,
	// including all its postings, or ErrTransactionNotFound if none exists.
	GetTransaction(ctx context.Context, tenantID, id string) (Transaction, error)

	// Balance returns the derived balance of an account: the sum of its postings'
	// signed amounts. It returns ErrAccountNotFound if the account does not exist.
	Balance(ctx context.Context, tenantID, accountID string) (Money, error)
}
