package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestMalformedIDsReturnErrors drives every repository method that parses a
// tenant, account, transaction, or cursor id, feeding each a syntactically
// invalid UUID. The API layer already rejects malformed ids before they reach
// here, but the adapter must not trust that blindly: every uuid.Parse call is
// a real branch, and a caller bypassing the API (another service, a bug
// upstream) must get a clean error, not a panic or silently wrong data.
func TestMalformedIDsReturnErrors(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	const bad = "not-a-uuid"

	tests := []struct {
		name string
		call func() error
	}{
		{"CreateAccount bad tenant", func() error {
			return repo.CreateAccount(ctx, bad, &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"})
		}},
		{"CreateAccount bad preset account id", func() error {
			acct := &domain.Account{ID: bad, Name: "Cash", Type: domain.Asset, Currency: "USD"}
			return repo.CreateAccount(ctx, uuid.NewString(), acct)
		}},
		{"GetAccount bad tenant", func() error {
			_, err := repo.GetAccount(ctx, bad, uuid.NewString())
			return err
		}},
		{"GetAccount bad account id", func() error {
			_, err := repo.GetAccount(ctx, uuid.NewString(), bad)
			return err
		}},
		{"ListAccounts bad tenant", func() error {
			_, err := repo.ListAccounts(ctx, bad, 10)
			return err
		}},
		{"GetTransaction bad tenant", func() error {
			_, err := repo.GetTransaction(ctx, bad, uuid.NewString())
			return err
		}},
		{"GetTransaction bad transaction id", func() error {
			_, err := repo.GetTransaction(ctx, uuid.NewString(), bad)
			return err
		}},
		{"GetIdempotencyKey bad tenant", func() error {
			_, err := repo.GetIdempotencyKey(ctx, bad, "some-key")
			return err
		}},
		{"ListAuditByTransaction bad tenant", func() error {
			_, err := repo.ListAuditByTransaction(ctx, bad, uuid.NewString())
			return err
		}},
		{"ListAuditByTransaction bad transaction id", func() error {
			_, err := repo.ListAuditByTransaction(ctx, uuid.NewString(), bad)
			return err
		}},
		{"ListAuditByAccount bad tenant", func() error {
			_, err := repo.ListAuditByAccount(ctx, bad, uuid.NewString(), nil, 10)
			return err
		}},
		{"ListAuditByAccount bad account id", func() error {
			_, err := repo.ListAuditByAccount(ctx, uuid.NewString(), bad, nil, 10)
			return err
		}},
		{"ListAuditByAccount bad cursor id", func() error {
			cursor := &domain.StatementCursor{ID: bad}
			_, err := repo.ListAuditByAccount(ctx, uuid.NewString(), uuid.NewString(), cursor, 10)
			return err
		}},
		{"Statement bad tenant", func() error {
			_, err := repo.Statement(ctx, bad, uuid.NewString(), "USD", nil, 10)
			return err
		}},
		{"Statement bad account id", func() error {
			_, err := repo.Statement(ctx, uuid.NewString(), bad, "USD", nil, 10)
			return err
		}},
		{"Statement bad cursor id", func() error {
			cursor := &domain.StatementCursor{ID: bad}
			_, err := repo.Statement(ctx, uuid.NewString(), uuid.NewString(), "USD", cursor, 10)
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.call(); err == nil {
				t.Fatal("expected a parse error, got nil")
			}
		})
	}
}

// TestCreateAccountInvalidFailsValidate proves an account that fails
// domain validation (here, an empty name) never reaches the insert: the
// typed domain error comes back, not a database constraint violation.
func TestCreateAccountInvalidFailsValidate(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	acct := &domain.Account{Type: domain.Asset, Currency: "USD"} // no Name
	err := repo.CreateAccount(context.Background(), uuid.NewString(), acct)
	if !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("got %v, want ErrInvalidAccount", err)
	}
}

// TestGetTransactionNotFound proves a lookup for a transaction id that does
// not exist returns the typed domain.ErrTransactionNotFound, mirroring
// TestGetAccountNotFound in repository_test.go.
func TestGetTransactionNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	_, err := repo.GetTransaction(context.Background(), uuid.NewString(), uuid.NewString())
	if !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("got %v, want ErrTransactionNotFound", err)
	}
}

// TestBalanceAccountNotFound proves Balance surfaces ErrAccountNotFound (via
// its internal GetAccount call) rather than a bare "no rows" error when the
// account does not exist.
func TestBalanceAccountNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	_, err := repo.Balance(context.Background(), uuid.NewString(), uuid.NewString())
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("got %v, want ErrAccountNotFound", err)
	}
}

// TestCreateTransactionDuplicateID proves that posting two transactions with
// the same explicit id under one tenant surfaces the typed
// domain.ErrDuplicateTransaction (the postgres unique-violation mapping),
// rather than a generic wrapped pg error.
func TestCreateTransactionDuplicateID(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "duplicate txn id tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	a := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	b := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, a); err != nil {
		t.Fatalf("create account a: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, b); err != nil {
		t.Fatalf("create account b: %v", err)
	}

	fixedID := uuid.NewString()
	newTxn := func() *domain.Transaction {
		return &domain.Transaction{
			ID: fixedID,
			Postings: []domain.Posting{
				{AccountID: a.ID, Amount: money(t, 100, "USD")},
				{AccountID: b.ID, Amount: money(t, -100, "USD")},
			},
		}
	}

	if err := repo.CreateTransaction(ctx, tenant, newTxn()); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := repo.CreateTransaction(ctx, tenant, newTxn())
	if !errors.Is(err, domain.ErrDuplicateTransaction) {
		t.Fatalf("second create with same id: got %v, want ErrDuplicateTransaction", err)
	}
}

// TestCreateTransactionUnbalancedViaRunInTx bypasses the public
// Repository.CreateTransaction wrapper (which validates before starting the
// transaction) and calls RunInTx directly with an unbalanced transaction, the
// same way a bug in a caller other than the public entry point could. This
// exercises the deferred database balance trigger (assert_txn_balanced) firing
// at COMMIT, and RunInTx's mapping of that constraint violation to the typed
// domain.ErrUnbalanced, which the comment on RunInTx calls out as defense in
// depth.
func TestCreateTransactionUnbalancedViaRunInTx(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "unbalanced runintx tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	a := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, a); err != nil {
		t.Fatalf("create account: %v", err)
	}

	unbalanced := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: a.ID, Amount: money(t, 100, "USD")},
	}}
	err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.CreateTransaction(ctx, tenant, unbalanced)
	})
	if !errors.Is(err, domain.ErrUnbalanced) {
		t.Fatalf("got %v, want ErrUnbalanced", err)
	}
}

// TestCreateTransactionBadPostingAccountID proves a posting with a
// syntactically invalid account id fails to parse rather than being sent to
// the database.
func TestCreateTransactionBadPostingAccountID(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: "not-a-uuid", Amount: money(t, 100, "USD")},
		{AccountID: uuid.NewString(), Amount: money(t, -100, "USD")},
	}}
	err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.CreateTransaction(ctx, tenant, txn)
	})
	if err == nil {
		t.Fatal("expected a parse error for the malformed posting account id, got nil")
	}
}

// TestCreateTransactionPostingAccountMissing proves a posting into an account
// id that is well-formed but does not exist surfaces a generic wrapped error
// (a foreign-key violation), the fallback branch below the
// postings_currency_matches mapping.
func TestCreateTransactionPostingAccountMissing(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "missing posting account tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	existing := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, existing); err != nil {
		t.Fatalf("create account: %v", err)
	}

	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: existing.ID, Amount: money(t, 100, "USD")},
		{AccountID: uuid.NewString(), Amount: money(t, -100, "USD")}, // no such account
	}}
	err := repo.CreateTransaction(ctx, tenant, txn)
	if err == nil {
		t.Fatal("expected an error posting into a nonexistent account, got nil")
	}
	if errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Fatal("a missing account should not be reported as a currency mismatch")
	}
}

// TestInsertIdempotencyKeyEdges drives InsertIdempotencyKey's malformed-id and
// generic-error branches: a bad tenant id, a bad transaction id, and a
// well-formed but nonexistent transaction id (a foreign-key violation, the
// fallback branch below the idempotency_keys_pkey mapping already covered by
// TestIdempotencyKeyInsertAndDuplicate in idempotency_audit_test.go).
func TestInsertIdempotencyKeyEdges(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	t.Run("bad tenant", func(t *testing.T) {
		t.Parallel()
		err := repo.RunInTx(ctx, uuid.NewString(), func(ctx context.Context, tx domain.Tx) error {
			return tx.InsertIdempotencyKey(ctx, "not-a-uuid", "k", "fp", "v1", uuid.NewString())
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("bad transaction id", func(t *testing.T) {
		t.Parallel()
		err := repo.RunInTx(ctx, uuid.NewString(), func(ctx context.Context, tx domain.Tx) error {
			return tx.InsertIdempotencyKey(ctx, uuid.NewString(), "k", "fp", "v1", "not-a-uuid")
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("transaction does not exist", func(t *testing.T) {
		t.Parallel()
		err := repo.RunInTx(ctx, uuid.NewString(), func(ctx context.Context, tx domain.Tx) error {
			return tx.InsertIdempotencyKey(ctx, uuid.NewString(), "k", "fp", "v1", uuid.NewString())
		})
		if err == nil {
			t.Fatal("expected a foreign-key error, got nil")
		}
		if errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
			t.Fatal("a missing transaction should not be reported as a duplicate key")
		}
	})
}

// TestAppendAuditOutboxEdges drives AppendAuditOutbox's malformed-id and
// generic-error branches: a bad tenant id, a bad transaction id, and a
// well-formed but nonexistent transaction id (a foreign-key violation:
// audit_outbox.transaction_id references transactions(id), migration 0015).
func TestAppendAuditOutboxEdges(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	t.Run("bad tenant", func(t *testing.T) {
		t.Parallel()
		err := repo.RunInTx(ctx, uuid.NewString(), func(ctx context.Context, tx domain.Tx) error {
			return tx.AppendAuditOutbox(ctx, "not-a-uuid", domain.AuditEvent{
				Action:        domain.ActionTransactionCreated,
				TransactionID: uuid.NewString(),
				Actor:         "actor",
				After:         []byte(`{}`),
			})
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("bad transaction id", func(t *testing.T) {
		t.Parallel()
		err := repo.RunInTx(ctx, uuid.NewString(), func(ctx context.Context, tx domain.Tx) error {
			return tx.AppendAuditOutbox(ctx, uuid.NewString(), domain.AuditEvent{
				Action:        domain.ActionTransactionCreated,
				TransactionID: "not-a-uuid",
				Actor:         "actor",
				After:         []byte(`{}`),
			})
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("transaction does not exist", func(t *testing.T) {
		t.Parallel()
		err := repo.RunInTx(ctx, uuid.NewString(), func(ctx context.Context, tx domain.Tx) error {
			return tx.AppendAuditOutbox(ctx, uuid.NewString(), domain.AuditEvent{
				Action:        domain.ActionTransactionCreated,
				TransactionID: uuid.NewString(),
				Actor:         "actor",
				After:         []byte(`{}`),
			})
		})
		if err == nil {
			t.Fatal("expected a foreign-key error, got nil")
		}
	})
}

// TestRunInTxCancelledDuringBackoff proves that a context cancelled while
// RunInTx is waiting out its retry backoff returns ctx.Err() promptly, rather
// than blocking for the full backoff or retrying again. The fn cancels its own
// context right after handing back a serialization failure on the first
// attempt, guaranteeing the context is already done by the time the loop
// reaches the backoff wait for attempt 2.
func TestRunInTxCancelledDuringBackoff(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := repo.RunInTx(ctx, uuid.NewString(), func(_ context.Context, _ domain.Tx) error {
		calls++
		if calls == 1 {
			cancel()
			return serErr()
		}
		t.Fatal("fn should not be invoked again after the context was cancelled")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call before the cancellation was observed, got %d", calls)
	}
}

// TestNewPoolInvalidDSN proves NewPool surfaces a wrapped error for a DSN that
// fails to parse, instead of panicking or returning a half-built pool.
func TestNewPoolInvalidDSN(t *testing.T) {
	t.Parallel()
	_, err := postgres.NewPool(context.Background(), "://not a dsn", 5)
	if err == nil {
		t.Fatal("expected an error for an invalid DSN, got nil")
	}
}

// TestNewPoolDefaultsMaxConns proves maxConns <= 0 falls back to the package
// default rather than configuring a zero-sized (unusable) pool. The DSN
// parses but is never dialed: pgxpool connects lazily, so this does not need a
// live Postgres.
func TestNewPoolDefaultsMaxConns(t *testing.T) {
	t.Parallel()
	pool, err := postgres.NewPool(context.Background(), "postgres://user:pass@127.0.0.1:5", 0)
	if err != nil {
		t.Fatalf("NewPool with maxConns=0: %v", err)
	}
	defer pool.Close()
	if pool.Config().MaxConns <= 0 {
		t.Errorf("MaxConns = %d, want the package default applied", pool.Config().MaxConns)
	}
}
