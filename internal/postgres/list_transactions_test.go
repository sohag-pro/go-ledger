package postgres_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// seedListTransactions posts n balanced transactions between cash and other,
// each carrying a distinct reference ("list-txn-ref-<i>") and a small sleep
// between posts so created_at is strictly increasing across them (real wall
// clock time between round trips, the same margin TestAccountStatement
// relies on implicitly via sequential posts). It returns the posted ids in
// posting order (oldest first).
func seedListTransactions(t *testing.T, repo *postgres.Repository, tenant string, cash, other *domain.Account, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		debit, err := domain.NewMoney(int64(100+i), "USD")
		if err != nil {
			t.Fatalf("new money: %v", err)
		}
		credit, err := domain.NewMoney(-int64(100+i), "USD")
		if err != nil {
			t.Fatalf("new money: %v", err)
		}
		ref := fmt.Sprintf("list-txn-ref-%d", i)
		txn := &domain.Transaction{
			Postings: []domain.Posting{
				{AccountID: cash.ID, Amount: debit, Description: "seed"},
				{AccountID: other.ID, Amount: credit},
			},
			Reference: &ref,
		}
		if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		ids[i] = txn.ID
		time.Sleep(2 * time.Millisecond)
	}
	return ids
}

// TestListTransactions exercises ListTransactions against real Postgres:
// unfiltered ordering (newest first) with postings batch-loaded and carrying
// their own ids, an exact reference match, and a from/to date-range window
// (Task 4.4, audit A7.2).
func TestListTransactions(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "list transactions tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	other := &domain.Account{Name: "Other", Type: domain.Asset, Currency: "USD"}
	for _, a := range []*domain.Account{cash, other} {
		if err := repo.CreateAccount(ctx, tenant, a); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}

	const n = 5
	ids := seedListTransactions(t, repo, tenant, cash, other, n)

	// Unfiltered: all n, newest first, each with both postings, each posting
	// carrying its own id (not just its parent transaction's).
	all, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, nil, n+1)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != n {
		t.Fatalf("got %d transactions, want %d", len(all), n)
	}
	for i, item := range all {
		wantID := ids[n-1-i]
		if item.Transaction.ID != wantID {
			t.Errorf("all[%d].ID = %s, want %s (newest first)", i, item.Transaction.ID, wantID)
		}
		if len(item.Transaction.Postings) != 2 {
			t.Fatalf("all[%d] has %d postings, want 2", i, len(item.Transaction.Postings))
		}
		for _, p := range item.Transaction.Postings {
			if p.ID == "" {
				t.Errorf("all[%d]: posting missing its own id", i)
			}
		}
	}

	// Exact reference match returns exactly the one transaction that carries it.
	ref2 := "list-txn-ref-2"
	byRef, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{Reference: &ref2}, nil, 10)
	if err != nil {
		t.Fatalf("list by reference: %v", err)
	}
	if len(byRef) != 1 || byRef[0].Transaction.ID != ids[2] {
		t.Fatalf("reference filter = %+v, want exactly transaction %s", byRef, ids[2])
	}

	// from/to brackets around ids[1..3]: from is ids[1]'s own created_at
	// (inclusive), to is ids[4]'s (exclusive), read back from the unfiltered
	// page above so the boundary is the database's own clock, not a guess.
	from := all[n-1-1].CreatedAt // ids[1]
	to := all[n-1-4].CreatedAt   // ids[4]
	ranged, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{From: &from, To: &to}, nil, 10)
	if err != nil {
		t.Fatalf("list by range: %v", err)
	}
	wantRanged := []string{ids[3], ids[2], ids[1]} // newest first within [1,4)
	gotRanged := make([]string, 0, len(ranged))
	for _, item := range ranged {
		gotRanged = append(gotRanged, item.Transaction.ID)
	}
	if !reflect.DeepEqual(gotRanged, wantRanged) {
		t.Errorf("range filter = %v, want %v", gotRanged, wantRanged)
	}
}

// TestListTransactionsPagination walks ListTransactions with a page size
// that does not evenly divide the seeded count, keyset paging via the last
// entry of each page, and checks the walk covers every transaction exactly
// once, in newest-first order, with no gap or overlap, and terminates
// (a short final page) rather than looping forever (Task 4.4, audit A7.2).
func TestListTransactionsPagination(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "list transactions pagination tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	other := &domain.Account{Name: "Other", Type: domain.Asset, Currency: "USD"}
	for _, a := range []*domain.Account{cash, other} {
		if err := repo.CreateAccount(ctx, tenant, a); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}

	const n = 5
	ids := seedListTransactions(t, repo, tenant, cash, other, n)

	const pageSize = 2
	seen := map[string]bool{}
	var walked []string
	var cursor *domain.StatementCursor
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatalf("pagination did not terminate after %d pages", pages)
		}
		page, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, cursor, pageSize)
		if err != nil {
			t.Fatalf("list page %d: %v", pages, err)
		}
		for _, item := range page {
			if seen[item.Transaction.ID] {
				t.Fatalf("transaction %s returned twice across pages (overlap)", item.Transaction.ID)
			}
			seen[item.Transaction.ID] = true
			walked = append(walked, item.Transaction.ID)
		}
		if len(page) < pageSize {
			break // short page: this was the last one
		}
		last := page[len(page)-1]
		cursor = &domain.StatementCursor{CreatedAt: last.CreatedAt, ID: last.Transaction.ID}
	}

	wantOrder := make([]string, n)
	for i := 0; i < n; i++ {
		wantOrder[i] = ids[n-1-i]
	}
	if !reflect.DeepEqual(walked, wantOrder) {
		t.Fatalf("walked order = %v, want %v (no gap, no overlap, newest first)", walked, wantOrder)
	}
}

// TestListTransactionsTenantIsolation proves a tenant never sees another
// tenant's transactions through ListTransactions, even unfiltered.
func TestListTransactionsTenantIsolation(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	owner := uuid.NewString()
	other := uuid.NewString()
	if err := repo.CreateTenant(ctx, owner, "owner tenant"); err != nil {
		t.Fatalf("create owner tenant: %v", err)
	}
	if err := repo.CreateTenant(ctx, other, "other tenant"); err != nil {
		t.Fatalf("create other tenant: %v", err)
	}

	ownerCash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	ownerOther := &domain.Account{Name: "Other", Type: domain.Asset, Currency: "USD"}
	otherCash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	otherOther := &domain.Account{Name: "Other", Type: domain.Asset, Currency: "USD"}
	for tenant, accts := range map[string][]*domain.Account{
		owner: {ownerCash, ownerOther},
		other: {otherCash, otherOther},
	} {
		for _, a := range accts {
			if err := repo.CreateAccount(ctx, tenant, a); err != nil {
				t.Fatalf("create account: %v", err)
			}
		}
	}

	ownerIDs := seedListTransactions(t, repo, owner, ownerCash, ownerOther, 2)
	otherIDs := seedListTransactions(t, repo, other, otherCash, otherOther, 1)

	ownerList, err := repo.ListTransactions(ctx, owner, domain.TransactionFilter{}, nil, 100)
	if err != nil {
		t.Fatalf("list owner: %v", err)
	}
	if len(ownerList) != len(ownerIDs) {
		t.Fatalf("owner sees %d transactions, want %d", len(ownerList), len(ownerIDs))
	}
	for _, item := range ownerList {
		for _, id := range otherIDs {
			if item.Transaction.ID == id {
				t.Fatalf("owner tenant sees other tenant's transaction %s (cross-tenant leak)", id)
			}
		}
	}

	otherList, err := repo.ListTransactions(ctx, other, domain.TransactionFilter{}, nil, 100)
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(otherList) != len(otherIDs) || otherList[0].Transaction.ID != otherIDs[0] {
		t.Fatalf("other tenant list = %+v, want exactly its own transaction %s", otherList, otherIDs[0])
	}
}
