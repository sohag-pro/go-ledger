package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// stubStatementExportRepo is a minimal domain.Repository test double for
// exercising AccountService.StatementExport's cap/truncation logic without a
// database (Task 6.3, audit A9.2), the same pattern stubListRepo
// (list_export_test.go) uses for TransactionService.ExportTransactions.
type stubStatementExportRepo struct {
	domain.Repository

	acct domain.Account

	entries []domain.StatementEntry

	gotFrom, gotTo *time.Time
	gotLimit       int
}

func (s *stubStatementExportRepo) GetAccount(_ context.Context, _, _ string) (domain.Account, error) {
	return s.acct, nil
}

func (s *stubStatementExportRepo) StatementExport(_ context.Context, _, _ string, _ domain.Currency, from, to *time.Time, limit int) ([]domain.StatementEntry, error) {
	s.gotFrom, s.gotTo, s.gotLimit = from, to, limit
	if limit > 0 && len(s.entries) > limit {
		return s.entries[:limit], nil
	}
	return s.entries, nil
}

func mkStatementEntries(n int) []domain.StatementEntry {
	entries := make([]domain.StatementEntry, n)
	for i := range entries {
		amt, _ := domain.NewMoney(int64(i), "USD")
		entries[i] = domain.StatementEntry{ID: "posting-" + time.Unix(int64(i), 0).UTC().String(), Amount: amt, CreatedAt: time.Unix(int64(i), 0).UTC()}
	}
	return entries
}

// TestAccountService_StatementExport_NotTruncated checks the untruncated
// case: fewer entries than ledger.MaxExportRows are all returned, truncated
// is false, and the repository was asked for a limit of MaxExportRows+1 (the
// same detect-truncation-without-a-second-round-trip convention
// ExportTransactions uses).
func TestAccountService_StatementExport_NotTruncated(t *testing.T) {
	t.Parallel()
	repo := &stubStatementExportRepo{
		acct:    domain.Account{ID: "acct-1", Currency: "USD"},
		entries: mkStatementEntries(5),
	}
	svc := ledger.NewAccountService(repo)

	from := time.Now().Add(-time.Hour)
	_, entries, truncated, err := svc.StatementExport(context.Background(), "tenant-1", "acct-1", &from, nil)
	if err != nil {
		t.Fatalf("StatementExport: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false")
	}
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(entries))
	}
	if repo.gotLimit != ledger.MaxExportRows+1 {
		t.Errorf("limit = %d, want %d", repo.gotLimit, ledger.MaxExportRows+1)
	}
	if repo.gotFrom != &from {
		t.Error("from not passed through unchanged")
	}
	if repo.gotTo != nil {
		t.Errorf("to = %v, want nil", repo.gotTo)
	}
}

// TestAccountService_StatementExport_Truncated checks the cap: when the
// account's matching posting history exceeds ledger.MaxExportRows, the
// export contains exactly MaxExportRows entries (the newest ones, since
// StatementExport itself pages newest first) and truncated is true.
func TestAccountService_StatementExport_Truncated(t *testing.T) {
	t.Parallel()
	repo := &stubStatementExportRepo{
		acct:    domain.Account{ID: "acct-1", Currency: "USD"},
		entries: mkStatementEntries(ledger.MaxExportRows + 1),
	}
	svc := ledger.NewAccountService(repo)

	_, entries, truncated, err := svc.StatementExport(context.Background(), "tenant-1", "acct-1", nil, nil)
	if err != nil {
		t.Fatalf("StatementExport: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true")
	}
	if len(entries) != ledger.MaxExportRows {
		t.Fatalf("got %d entries, want %d", len(entries), ledger.MaxExportRows)
	}
}
