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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// SetFXRate appends a tenant-scoped fx_rates row for tenantID (Task 2.4,
// audit A3.3), so that tenant's own rate and spread for (base, quote) is
// resolved ahead of the global default (see fx.Provider.Rate). Unlike
// IssueKey, this does not require tenantID to be active: an operator may
// want to configure a rate for a tenant before or during a status change.
// It returns domain.ErrTenantNotFound if tenantID does not exist, and the
// same validation errors domain.Repository.InsertFXRate documents for a
// malformed base/quote/midRateE8/spreadBps.
//
// effectiveAt is nil for "effective immediately": the repository lets the
// database server's own clock stamp the row rather than defaulting to this
// process's time.Now() (Task 2.4 remediation; see
// domain.Repository.InsertFXRate). A non-nil effectiveAt (a scheduled,
// possibly future rate) is passed through unchanged.
func (s *Service) SetFXRate(ctx context.Context, tenantID string, base, quote domain.Currency, midRateE8 int64, spreadBps int32, source string, effectiveAt *time.Time) error {
	return s.repo.InsertFXRate(ctx, &tenantID, base, quote, midRateE8, spreadBps, source, effectiveAt)
}

// SetTenantPolicy writes policy into tenantID's tenants.settings jsonb
// column as {"policy": {...}} (Task 2.4b, audit A3.4), replacing whatever
// was there before: the settings document holds nothing else yet, so a
// whole-document write is the same thing as an update, without needing a
// read-modify-write.
//
// policy is validated first (TenantPolicy.Validate: no negative limit, every
// AllowedCurrencies entry a well-formed three-letter code), the same
// defense-in-depth style validateScopes and requireActiveTenant apply
// elsewhere in this file, so a malformed policy is rejected with
// domain.ErrInvalidTenantPolicy before anything is written. It returns
// domain.ErrTenantNotFound if tenantID has no row.
//
// Unlike IssueKey, this does not require the tenant to be active: an
// operator may want to configure guardrails ahead of, or during, a status
// change, exactly like SetFXRate above.
func (s *Service) SetTenantPolicy(ctx context.Context, tenantID string, policy domain.TenantPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(domain.TenantSettings{Policy: policy})
	if err != nil {
		return fmt.Errorf("admin: marshal tenant policy: %w", err)
	}
	return s.repo.SetTenantSettings(ctx, tenantID, raw)
}

// CreateWebhookSubscription mints a new webhook subscription for tenantID
// (Task 4.1, audit A7.1): it validates url before ever generating a secret
// or writing a row (domain.WebhookSubscription.Validate, ErrInvalidWebhookURL
// for anything that is not an absolute http/https URL), generates a fresh
// CSPRNG signing secret (domain.GenerateWebhookSecret), and returns that
// secret exactly once, alongside the stored subscription. It returns
// domain.ErrTenantNotFound if tenantID names a tenant that does not exist.
// Unlike IssueKey, it does not require the tenant to be active: an operator
// may want to configure a subscription ahead of, or during, a status
// change, the same reasoning SetFXRate and SetTenantPolicy already
// document for their own calls.
func (s *Service) CreateWebhookSubscription(ctx context.Context, tenantID, url string, eventTypes []string) (string, domain.WebhookSubscription, error) {
	sub := domain.WebhookSubscription{TenantID: tenantID, URL: url, EventTypes: eventTypes}
	if err := sub.Validate(); err != nil {
		return "", domain.WebhookSubscription{}, err
	}
	secret, err := domain.GenerateWebhookSecret()
	if err != nil {
		return "", domain.WebhookSubscription{}, fmt.Errorf("admin: generate webhook secret: %w", err)
	}
	if err := s.repo.CreateWebhookSubscription(ctx, &sub, secret); err != nil {
		return "", domain.WebhookSubscription{}, err
	}
	return secret, sub, nil
}

// ListWebhookSubscriptions returns every subscription for tenantID, oldest
// first, active or not. It never returns a secret: that is shown once, at
// CreateWebhookSubscription time, and domain.WebhookSubscription itself
// carries no field capable of holding one.
func (s *Service) ListWebhookSubscriptions(ctx context.Context, tenantID string) ([]domain.WebhookSubscription, error) {
	return s.repo.ListWebhookSubscriptionsByTenant(ctx, tenantID)
}

// ShredTenantPII irreversibly destroys tenantID's PII encryption key (Task
// 6.2, audit A9.3): the crypto-shredding operation behind
// POST /v1/admin/tenants/{id}/shred-pii and `ledgerctl tenant shred-pii`.
// Every posting description that tenant ever had encrypted
// (internal/crypto.Cipher, wired in only when LEDGER_MASTER_KEY is set)
// becomes permanently unreadable: a later read decrypts to
// crypto.RedactedMarker instead of erroring. Money data (accounts,
// transactions, postings' amounts, balances) and the tamper-evident audit
// hash chain (ADR-012) are completely untouched: crypto_keys is a separate
// table from every one of those, and the chain hashes the exact ciphertext
// bytes already stored in audit_log.after, never decrypts them, so it
// verifies identically before and after this call.
//
// This is a direct, thin pass-through to
// domain.Repository.ShredTenantCryptoKey, the same "no separate persistence
// dependency" shape every other method in this file follows: it does not
// require tenantID to exist or be active first (unlike IssueKey), mirroring
// SetFXRate/SetTenantPolicy/CreateWebhookSubscription's own reasoning, an
// operator may be shredding PII as part of, or ahead of, closing a tenant
// entirely. It is idempotent: calling it again for an already-shredded
// tenant is a no-op success, not an error, and never moves the original
// erasure timestamp (see ShredTenantCryptoKey's own doc comment).
//
// THIS IS IRREVERSIBLE. There is no undo: once a tenant's key is destroyed,
// none of its encrypted descriptions can ever be recovered, by anyone,
// including this codebase's own operators. See
// docs/ops/retention-and-erasure.md.
func (s *Service) ShredTenantPII(ctx context.Context, tenantID string) error {
	if err := s.repo.ShredTenantCryptoKey(ctx, tenantID); err != nil {
		return err
	}
	// Logged as a distinct, irreversible ops event (never plaintext, never a
	// key: there is neither to log here, only the fact that this tenant's
	// key no longer exists), regardless of which surface called this
	// (REST or ledgerctl): this is the one choke point both go through.
	slog.WarnContext(ctx, "tenant pii crypto-shredded", "tenant_id", tenantID)
	return nil
}

// DeleteWebhookSubscription deactivates the subscription identified by id
// (Task 4.1): it stops the fan-out worker from creating any further pending
// deliveries for it and stops the delivery worker from attempting any of its
// already-pending rows, without discarding delivery history (see
// domain.Repository.SetWebhookSubscriptionActive's doc comment for why this
// is a deactivate, not a hard delete). It returns
// domain.ErrWebhookSubscriptionNotFound if no subscription matches id.
func (s *Service) DeleteWebhookSubscription(ctx context.Context, id string) error {
	return s.repo.SetWebhookSubscriptionActive(ctx, id, false)
}
