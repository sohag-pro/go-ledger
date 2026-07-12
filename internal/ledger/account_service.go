package ledger

import (
	"context"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// AccountService is the application service for accounts: creation, reads, and
// statements. Like TransactionService it is thin, delegating persistence to the
// repository port and holding no SQL.
type AccountService struct {
	repo            domain.Repository
	defaultCurrency domain.Currency
	cipher          DescriptionCipher
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

// WithAccountCipher sets the DescriptionCipher Statement uses to decrypt a
// posting's description on read (Task 6.2, audit A9.3). Without this option
// the cipher is nil (encryption disabled), so Statement returns descriptions
// exactly as stored, matching behavior before Task 6.2.
func WithAccountCipher(c DescriptionCipher) AccountOption {
	return func(s *AccountService) { s.cipher = c }
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
// than a validation error. parentID (ADR-023) sets a's hierarchy parent; nil
// creates a root account, same as before ParentID existed. A cycle, currency
// mismatch, or unknown parent is enforced in Postgres and surfaces from the
// repo as ErrInvalidHierarchy / ErrParentNotFound.
func (s *AccountService) Create(ctx context.Context, tenantID string, a *domain.Account, parentID *string) error {
	if a.Currency == "" && s.defaultCurrency != "" {
		a.Currency = s.defaultCurrency
	}
	a.ParentID = parentID
	return s.repo.CreateAccount(ctx, tenantID, a)
}

// Get returns an account, or domain.ErrAccountNotFound.
func (s *AccountService) Get(ctx context.Context, tenantID, id string) (domain.Account, error) {
	return s.repo.GetAccount(ctx, tenantID, id)
}

// SetParent sets, changes, or clears (parentID nil) accountID's parent, then
// returns the updated account. A missing account is ErrAccountNotFound; a
// cycle, currency mismatch, or unknown parent surfaces from the repo as
// ErrInvalidHierarchy / ErrParentNotFound.
func (s *AccountService) SetParent(ctx context.Context, tenantID, accountID string, parentID *string) (domain.Account, error) {
	n, err := s.repo.SetAccountParent(ctx, tenantID, accountID, parentID)
	if err != nil {
		return domain.Account{}, err
	}
	if n == 0 {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	return s.repo.GetAccount(ctx, tenantID, accountID)
}

// RolledUpBalance returns accountID's balance including all descendants.
func (s *AccountService) RolledUpBalance(ctx context.Context, tenantID, accountID string) (domain.Money, error) {
	return s.repo.RolledUpBalance(ctx, tenantID, accountID)
}

// Tree returns the tenant's accounts as hierarchy nodes: own balance, rolled-up
// balance (own plus all descendants), and depth, ordered so each parent comes
// before its children. Rollups are computed in memory in O(n) from the flat
// own-balance list, so a deep tree costs one query, not one per node.
func (s *AccountService) Tree(ctx context.Context, tenantID string) ([]domain.AccountNode, error) {
	rows, err := s.repo.AllAccountBalances(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return buildTree(rows), nil
}

// buildTree threads the flat rows into parent-before-child order and rolls up
// each subtree once. Any row whose parent is missing (should not happen) is
// treated as a root, so the function never drops an account.
func buildTree(rows []domain.AccountBalanceRow) []domain.AccountNode {
	own := make(map[string]int64, len(rows))
	children := make(map[string][]string, len(rows))
	acctByID := make(map[string]domain.Account, len(rows))
	var roots []string
	for _, r := range rows {
		own[r.Account.ID] = r.Balance
		acctByID[r.Account.ID] = r.Account
		if r.Account.ParentID == nil {
			roots = append(roots, r.Account.ID)
		} else {
			children[*r.Account.ParentID] = append(children[*r.Account.ParentID], r.Account.ID)
		}
	}
	// A parent named by a child but absent from the map cannot happen (FK), but
	// guard anyway: promote orphans to roots.
	present := make(map[string]bool, len(rows))
	for id := range acctByID {
		present[id] = true
	}
	for _, r := range rows {
		if r.Account.ParentID != nil && !present[*r.Account.ParentID] {
			roots = append(roots, r.Account.ID)
		}
	}

	// rollup(id) = own + sum(rollup(child)); memoized.
	rolled := make(map[string]int64, len(rows))
	var rollup func(id string) int64
	rollup = func(id string) int64 {
		if v, ok := rolled[id]; ok {
			return v
		}
		total := own[id]
		for _, c := range children[id] {
			total += rollup(c)
		}
		rolled[id] = total
		return total
	}

	var out []domain.AccountNode
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		out = append(out, domain.AccountNode{
			Account:         acctByID[id],
			OwnBalance:      own[id],
			RolledUpBalance: rollup(id),
			Depth:           depth,
		})
		for _, c := range children[id] {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return out
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
	// Decrypt each entry's Description (Task 6.2, audit A9.3): a nil cipher
	// (encryption disabled) leaves entries completely unchanged.
	if s.cipher != nil {
		for i := range entries {
			if entries[i].Description == "" {
				continue
			}
			plaintext, err := s.cipher.Decrypt(ctx, tenantID, entries[i].Description)
			if err != nil {
				return domain.Account{}, nil, err
			}
			entries[i].Description = plaintext
		}
	}
	return acct, entries, nil
}

// StatementExport returns the account plus up to MaxExportRows of its
// postings within an optional [from, to) created_at window, newest first,
// each with its running balance (Task 6.3, audit A9.2): the per-account
// period statement export, bounded like ExportTransactions rather than
// keyset paged like Statement (and, like ExportTransactions, not caller
// configurable: MaxExportRows is the same fixed ceiling both exports share).
// truncated is true when the account's matching posting history within the
// window exceeds MaxExportRows, in which case the export contains only the
// newest MaxExportRows entries; the caller (the REST handler) surfaces it
// via the same Export-Truncated response header the transaction export
// uses.
func (s *AccountService) StatementExport(ctx context.Context, tenantID, id string, from, to *time.Time) (domain.Account, []domain.StatementEntry, bool, error) {
	acct, err := s.repo.GetAccount(ctx, tenantID, id)
	if err != nil {
		return domain.Account{}, nil, false, err
	}
	entries, err := s.repo.StatementExport(ctx, tenantID, id, acct.Currency, from, to, MaxExportRows+1)
	if err != nil {
		return domain.Account{}, nil, false, err
	}
	truncated := false
	if len(entries) > MaxExportRows {
		entries = entries[:MaxExportRows]
		truncated = true
	}
	// Decrypt each entry's Description (Task 6.2, audit A9.3), the same
	// pass Statement above applies: a nil cipher (encryption disabled)
	// leaves entries completely unchanged.
	if s.cipher != nil {
		for i := range entries {
			if entries[i].Description == "" {
				continue
			}
			plaintext, err := s.cipher.Decrypt(ctx, tenantID, entries[i].Description)
			if err != nil {
				return domain.Account{}, nil, false, err
			}
			entries[i].Description = plaintext
		}
	}
	return acct, entries, truncated, nil
}
