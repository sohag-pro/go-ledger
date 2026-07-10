// Package admin is the operator-facing service behind the /v1/admin REST
// surface and the ledgerctl CLI (Task 2.2b, audit A3.2/A2.3): onboarding a
// tenant and issuing, rotating, or revoking its API keys, with no raw SQL.
//
// It is a thin layer over domain.Repository: every method here either calls
// straight through to a Task 2.1/2.2 repository method (CreateTenant,
// ListTenants, SetTenantStatus) or composes domain.GenerateAPIKey with
// InsertAPIKey/GetAPIKeyByID/ListAPIKeysByTenant/RevokeAPIKey to mint, copy,
// list, or kill a key. It owns exactly two rules the repository does not:
// a key's scopes must be non-empty and every element valid, and a key may
// only be issued or rotated into a tenant that is active.
package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// ErrInvalidScopes is returned when IssueKey is asked to mint a key with no
// scopes, or with a scope that is not one of domain.Scope's three valid
// values. This is a fail-closed check the repository's own CHECK constraint
// (api_keys_scopes_valid, migration 0012) would also catch, but rejecting it
// here gives the REST and CLI callers a clean error instead of a raw
// constraint-violation message.
var ErrInvalidScopes = errors.New("admin: at least one valid scope is required")

// tenantListLimit bounds ListTenants: an operator tool, not a paged public
// listing, so a single generous cap is simpler than plumbing cursor paging
// through to the CLI and REST surface for what is expected to be at most a
// few hundred tenants.
const tenantListLimit = 1000

// Service is the admin surface's business logic, built directly over
// domain.Repository (the same port every other service in this codebase
// uses): it introduces no separate persistence dependency of its own.
type Service struct {
	repo domain.Repository
}

// NewService returns a Service backed by repo.
func NewService(repo domain.Repository) *Service {
	return &Service{repo: repo}
}

// CreateTenant creates a new, active tenant and returns it. The tenant id is
// generated here (a UUIDv7, the same identity scheme every other entity in
// this codebase uses) rather than round-tripped through the repository,
// since CreateTenant's repository method takes the id as an argument instead
// of assigning and returning one (see domain.Repository.CreateTenant).
func (s *Service) CreateTenant(ctx context.Context, name string) (domain.Tenant, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return domain.Tenant{}, fmt.Errorf("admin: generate tenant id: %w", err)
	}
	t := domain.Tenant{ID: id.String(), Name: name, Status: domain.TenantActive}
	if err := t.Validate(); err != nil {
		return domain.Tenant{}, err
	}
	if err := s.repo.CreateTenant(ctx, t.ID, t.Name); err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}

// ListTenants returns up to tenantListLimit tenants, oldest first.
func (s *Service) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	return s.repo.ListTenants(ctx, tenantListLimit)
}

// SetTenantStatus updates a tenant's lifecycle status (active, suspended, or
// closed). It returns domain.ErrInvalidTenant for an unrecognized status or
// domain.ErrTenantNotFound if tenantID has no row, both straight from the
// repository.
func (s *Service) SetTenantStatus(ctx context.Context, tenantID string, status domain.TenantStatus) error {
	return s.repo.SetTenantStatus(ctx, tenantID, status)
}

// validateScopes reports ErrInvalidScopes unless scopes is non-empty and
// every element is one of domain.Scope's three valid values.
func validateScopes(scopes []domain.Scope) error {
	if len(scopes) == 0 {
		return ErrInvalidScopes
	}
	for _, sc := range scopes {
		if !sc.Valid() {
			return ErrInvalidScopes
		}
	}
	return nil
}

// requireActiveTenant fetches tenantID and fails closed unless it is active:
// a missing tenant surfaces domain.ErrTenantNotFound, and a suspended or
// closed one surfaces a *domain.TenantNotActiveError (matched via
// errors.Is(err, domain.ErrTenantNotActive)), the same type the auth
// resolver uses for the identical rule at request time (Task 2.1, ADR-015).
// Reusing it here means a transport layer's existing mapping for that error
// (403, naming the reason) applies to the admin surface for free.
//
// Suspended is treated the same as closed, not just closed: a suspended
// tenant already cannot use any key it holds (the resolver gates every
// request on tenant status), so minting or rotating a new key into it would
// hand an operator a credential that does not work until the tenant is
// reactivated, which is more confusing than a clear error up front.
func (s *Service) requireActiveTenant(ctx context.Context, tenantID string) error {
	t, err := s.repo.GetTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	if t.Status != domain.TenantActive {
		return &domain.TenantNotActiveError{TenantID: tenantID, Status: t.Status}
	}
	return nil
}

// mintKey generates a fresh plaintext/hash pair and inserts a new api_keys
// row for tenantID with the given name, scopes, and optional expiry. The
// key's id is generated here (uuid.NewV7, the same scheme CreateTenant
// uses) so it is known to the caller without a round trip back through
// InsertAPIKey, which takes its domain.APIKey argument by value and so
// cannot write an assigned id back the way CreateAccount/CreateTransaction
// do.
func (s *Service) mintKey(ctx context.Context, tenantID, name string, scopes []domain.Scope, expiresAt *time.Time) (string, domain.APIKey, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", domain.APIKey{}, fmt.Errorf("admin: generate api key id: %w", err)
	}
	plaintext, hash, err := domain.GenerateAPIKey()
	if err != nil {
		return "", domain.APIKey{}, fmt.Errorf("admin: generate api key: %w", err)
	}
	key := domain.APIKey{
		ID:        id.String(),
		TenantID:  tenantID,
		Name:      name,
		Scopes:    scopes,
		ExpiresAt: expiresAt,
	}
	if err := s.repo.InsertAPIKey(ctx, key, hash); err != nil {
		return "", domain.APIKey{}, err
	}
	return plaintext, key, nil
}

// IssueKey mints a new key for tenantID with the given scopes and optional
// expiry, and returns the plaintext once (it is never stored; only its hash
// is) alongside the stored metadata. It fails closed: an empty or invalid
// scopes list returns ErrInvalidScopes, a missing tenant returns
// domain.ErrTenantNotFound, and a tenant that is not active returns a
// *domain.TenantNotActiveError, all before any key is generated.
func (s *Service) IssueKey(ctx context.Context, tenantID, name string, scopes []domain.Scope, expiresAt *time.Time) (string, domain.APIKey, error) {
	if err := validateScopes(scopes); err != nil {
		return "", domain.APIKey{}, err
	}
	if err := s.requireActiveTenant(ctx, tenantID); err != nil {
		return "", domain.APIKey{}, err
	}
	return s.mintKey(ctx, tenantID, name, scopes, expiresAt)
}

// RotateKey issues a new key carrying the same tenant, name, and scopes as
// the existing key identified by oldKeyID, and returns its plaintext. The
// old key is left untouched (still active): rotation is meant to open an
// overlap window so a caller can cut over to the new key before the old one
// is revoked, not to revoke it automatically. Call RevokeKey explicitly once
// the cutover is done.
//
// It returns domain.ErrAPIKeyNotFound if oldKeyID does not exist, and the
// same tenant-active gate as IssueKey (a missing or non-active tenant fails
// the rotation, even though the tenant id itself comes from the old key
// rather than a caller-supplied argument).
func (s *Service) RotateKey(ctx context.Context, oldKeyID string) (string, domain.APIKey, error) {
	old, err := s.repo.GetAPIKeyByID(ctx, oldKeyID)
	if err != nil {
		return "", domain.APIKey{}, err
	}
	if err := s.requireActiveTenant(ctx, old.TenantID); err != nil {
		return "", domain.APIKey{}, err
	}
	return s.mintKey(ctx, old.TenantID, old.Name, old.Scopes, old.ExpiresAt)
}

// RevokeKey revokes the key identified by id. Revoking an already-revoked
// key is a no-op success (see domain.Repository.RevokeAPIKey); it returns
// domain.ErrAPIKeyNotFound only if no key with that id exists at all.
func (s *Service) RevokeKey(ctx context.Context, keyID string) error {
	return s.repo.RevokeAPIKey(ctx, keyID)
}

// ListKeys returns every key belonging to tenantID, oldest first, revoked or
// not. It never returns a key's plaintext: that is never stored anywhere to
// return, only shown once at IssueKey/RotateKey time.
func (s *Service) ListKeys(ctx context.Context, tenantID string) ([]domain.APIKey, error) {
	return s.repo.ListAPIKeysByTenant(ctx, tenantID)
}
