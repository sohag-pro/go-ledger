package ledger_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// stubListRepo is a minimal domain.Repository test double for exercising
// TransactionService.ListTransactions and ExportTransactions without a
// database (Task 4.4, audit A7.2). It embeds a nil domain.Repository, the
// same pattern fakeSchemeRepo in fingerprint_scheme_test.go uses: any method
// this test does not expect to be called panics via the nil embed instead of
// silently proceeding. ListTransactions records the arguments it was called
// with and returns up to rows, capped at whatever limit it was asked for
// (mirroring the real repository's SQL LIMIT), so ExportTransactions's
// truncation logic can be exercised by simply handing it more rows than
// ledger.MaxExportRows without needing to actually create that many
// transactions.
type stubListRepo struct {
	domain.Repository

	rows []domain.TransactionListItem

	gotTenant string
	gotFilter domain.TransactionFilter
	gotAfter  *domain.StatementCursor
	gotLimit  int
}

func (s *stubListRepo) ListTransactions(_ context.Context, tenantID string, filter domain.TransactionFilter, after *domain.StatementCursor, limit int) ([]domain.TransactionListItem, error) {
	s.gotTenant = tenantID
	s.gotFilter = filter
	s.gotAfter = after
	s.gotLimit = limit
	if limit > 0 && len(s.rows) > limit {
		return s.rows[:limit], nil
	}
	return s.rows, nil
}

func mkListItems(n int) []domain.TransactionListItem {
	items := make([]domain.TransactionListItem, n)
	for i := range items {
		items[i] = domain.TransactionListItem{
			Transaction: domain.Transaction{ID: fmt.Sprintf("txn-%d", i)},
			CreatedAt:   time.Unix(int64(i), 0).UTC(),
		}
	}
	return items
}

// TestListTransactionsPassthrough proves ListTransactions is a thin
// read-through to the repository, the same shape AuditService.ByAccount and
// AccountService.Statement already are: the tenant, filter, cursor, and
// limit all reach the repository unchanged, and the repository's rows come
// back unchanged too.
func TestListTransactionsPassthrough(t *testing.T) {
	t.Parallel()
	from := time.Now().Add(-time.Hour)
	ref := "inv-1"
	filter := domain.TransactionFilter{From: &from, Reference: &ref}
	cursor := &domain.StatementCursor{ID: "cursor-id", CreatedAt: time.Now()}
	repo := &stubListRepo{rows: mkListItems(3)}
	svc := ledger.NewTransactionService(repo, nil, nil)

	got, err := svc.ListTransactions(context.Background(), "tenant-1", filter, cursor, 25)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	if repo.gotTenant != "tenant-1" {
		t.Errorf("tenant = %q, want tenant-1", repo.gotTenant)
	}
	if repo.gotLimit != 25 {
		t.Errorf("limit = %d, want 25", repo.gotLimit)
	}
	if repo.gotAfter != cursor {
		t.Errorf("cursor not passed through unchanged")
	}
	if repo.gotFilter.From != &from || repo.gotFilter.Reference != &ref {
		t.Errorf("filter not passed through unchanged: %+v", repo.gotFilter)
	}
}

// TestExportTransactionsNotTruncated checks the untruncated case: fewer rows
// than ledger.MaxExportRows are all returned, truncated is false, and the
// repository was asked for no cursor (export is not paged) and a limit of
// MaxExportRows+1 (so ExportTransactions itself can detect truncation the
// same way the list endpoints detect a next page).
func TestExportTransactionsNotTruncated(t *testing.T) {
	t.Parallel()
	repo := &stubListRepo{rows: mkListItems(5)}
	svc := ledger.NewTransactionService(repo, nil, nil)

	got, truncated, err := svc.ExportTransactions(context.Background(), "tenant-1", domain.TransactionFilter{})
	if err != nil {
		t.Fatalf("ExportTransactions: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false")
	}
	if len(got) != 5 {
		t.Fatalf("got %d rows, want 5", len(got))
	}
	if repo.gotAfter != nil {
		t.Errorf("export must not page from a cursor, got %+v", repo.gotAfter)
	}
	if repo.gotLimit != ledger.MaxExportRows+1 {
		t.Errorf("limit = %d, want %d", repo.gotLimit, ledger.MaxExportRows+1)
	}
}

// TestExportTransactionsTruncated checks the cap: when the tenant's matching
// history exceeds ledger.MaxExportRows, the export contains exactly
// MaxExportRows rows (the newest ones, since ListTransactions itself pages
// newest first) and truncated is true.
func TestExportTransactionsTruncated(t *testing.T) {
	t.Parallel()
	repo := &stubListRepo{rows: mkListItems(ledger.MaxExportRows + 1)}
	svc := ledger.NewTransactionService(repo, nil, nil)

	got, truncated, err := svc.ExportTransactions(context.Background(), "tenant-1", domain.TransactionFilter{})
	if err != nil {
		t.Fatalf("ExportTransactions: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true")
	}
	if len(got) != ledger.MaxExportRows {
		t.Fatalf("got %d rows, want %d", len(got), ledger.MaxExportRows)
	}
	// The first MaxExportRows of stub rows, i.e. the newest ones: stubListRepo
	// itself already trims to the requested limit (mirroring the real
	// repository's SQL LIMIT), so this also proves ExportTransactions did not
	// double-trim or drop the wrong end.
	if got[0].Transaction.ID != "txn-0" {
		t.Errorf("got[0].ID = %s, want txn-0 (the newest row kept)", got[0].Transaction.ID)
	}
}
