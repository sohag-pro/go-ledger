package ledger

import (
	"context"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// AccountService is the application service for accounts: creation, reads, and
// statements. Like TransactionService it is thin, delegating persistence to the
// repository port and holding no SQL.
type AccountService struct {
	repo domain.Repository
}

// NewAccountService returns an AccountService backed by repo.
func NewAccountService(repo domain.Repository) *AccountService {
	return &AccountService{repo: repo}
}

// Create persists a new account. The repository assigns an id if a.ID is empty
// and validates the account.
func (s *AccountService) Create(ctx context.Context, tenantID string, a *domain.Account) error {
	return s.repo.CreateAccount(ctx, tenantID, a)
}

// Get returns an account, or domain.ErrAccountNotFound.
func (s *AccountService) Get(ctx context.Context, tenantID, id string) (domain.Account, error) {
	return s.repo.GetAccount(ctx, tenantID, id)
}

// Balance returns an account's derived balance, or domain.ErrAccountNotFound.
func (s *AccountService) Balance(ctx context.Context, tenantID, id string) (domain.Money, error) {
	return s.repo.Balance(ctx, tenantID, id)
}

// Statement returns one keyset page of an account's postings, newest first, each
// with its running balance. It resolves the account first (for its currency and
// to return domain.ErrAccountNotFound when it is missing), then pages the
// postings. after is nil for the first page.
func (s *AccountService) Statement(ctx context.Context, tenantID, id string, after *domain.StatementCursor, limit int) (domain.Account, []domain.StatementEntry, error) {
	acct, err := s.repo.GetAccount(ctx, tenantID, id)
	if err != nil {
		return domain.Account{}, nil, err
	}
	entries, err := s.repo.Statement(ctx, tenantID, id, acct.Currency, after, limit)
	if err != nil {
		return domain.Account{}, nil, err
	}
	return acct, entries, nil
}
