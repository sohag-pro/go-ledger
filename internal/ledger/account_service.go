package ledger

import (
	"context"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// AccountService is the application service for accounts: creation, reads, and
// statements. Like TransactionService it is thin, delegating persistence to the
// repository port and holding no SQL.
type AccountService struct {
	repo            domain.Repository
	defaultCurrency domain.Currency
}

// AccountOption configures optional AccountService dependencies, mirroring
// TransactionService's ServiceOption: most existing callers and tests do not
// need one, so NewAccountService's required parameters stay unchanged.
type AccountOption func(*AccountService)

// WithDefaultCurrency sets the currency Create stamps on a new account when
// the caller does not specify one (ADR-014, "New-account default currency is
// env-configured"). Without this option, Create leaves an empty Currency
// as-is, which fails domain.Account.Validate with ErrInvalidCurrency, the
// same behavior as before this option existed.
func WithDefaultCurrency(c domain.Currency) AccountOption {
	return func(s *AccountService) { s.defaultCurrency = c }
}

// NewAccountService returns an AccountService backed by repo.
func NewAccountService(repo domain.Repository, opts ...AccountOption) *AccountService {
	s := &AccountService{repo: repo}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create persists a new account. The repository assigns an id if a.ID is empty
// and validates the account. If a.Currency is empty and WithDefaultCurrency was
// set, the configured default is stamped on before validation, so a caller that
// does not specify a currency gets the deployment's configured default rather
// than a validation error.
func (s *AccountService) Create(ctx context.Context, tenantID string, a *domain.Account) error {
	if a.Currency == "" && s.defaultCurrency != "" {
		a.Currency = s.defaultCurrency
	}
	return s.repo.CreateAccount(ctx, tenantID, a)
}

// Get returns an account, or domain.ErrAccountNotFound.
func (s *AccountService) Get(ctx context.Context, tenantID, id string) (domain.Account, error) {
	return s.repo.GetAccount(ctx, tenantID, id)
}

// SetStatus updates an account's lifecycle status (Task 5.5, audit A1.5:
// freeze, close, or reactivate one account) and returns the updated
// account. It returns domain.ErrInvalidAccount if status is not one of
// domain.AccountStatus.Valid()'s three values, or domain.ErrAccountNotFound
// if no account matches id. The new status takes effect for the NEXT
// posting attempt: it is read fresh, inside that posting's own SERIALIZABLE
// transaction (see internal/ledger's enforceAccountConstraints), never
// cached, so there is no window where a just-frozen account's in-flight
// posting is missed.
func (s *AccountService) SetStatus(ctx context.Context, tenantID, id string, status domain.AccountStatus) (domain.Account, error) {
	if err := s.repo.SetAccountStatus(ctx, tenantID, id, status); err != nil {
		return domain.Account{}, err
	}
	return s.repo.GetAccount(ctx, tenantID, id)
}

// List returns up to limit of the tenant's accounts, ordered by name.
func (s *AccountService) List(ctx context.Context, tenantID string, limit int) ([]domain.Account, error) {
	return s.repo.ListAccounts(ctx, tenantID, limit)
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
