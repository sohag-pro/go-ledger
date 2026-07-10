package ledger

import (
	"context"
	"errors"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// enforceTenantPolicy checks postings against policy (Task 2.4b, audit
// A3.4). It is called from inside RunInTx, in both Post and Convert, before
// CreateTransaction: the daily-volume check needs a read that is consistent
// with the write that follows it, and the per-tenant in-process
// serialization RunInTx already provides (ADR-012) is what makes that read
// race free.
//
// policy itself is NOT resolved in here: it must be resolved by the caller
// BEFORE RunInTx is ever invoked (see tenantPolicy), using the top-level
// Repository, not tx. RunInTx already holds one pooled connection open for
// the whole attempt (see Repository.RunInTx: it opens the transaction, then
// calls fn); a second, unrelated Repository call made from inside fn would
// need to check out a SECOND connection from the same pool while the first
// is still held. Under a small pool (internal/ledger's own
// TestPostNoCrossTenantStarvation runs with MaxConns=2 on purpose) two
// concurrent Post calls each needing a second connection while
// already holding the first is a genuine deadlock: neither can ever get its
// second connection because the other is holding it as their first. Keeping
// the settings read outside RunInTx (see Post and Convert) avoids that
// entirely, the same way Convert already resolves its accounts via
// GetAccount before ever calling RunInTx.
//
// A TenantDailyDebits read only happens when policy actually has a
// DailyVolumeLimit set: most tenants have no policy at all, and there is no
// reason to pay for an extra query on every single post for a check that
// would immediately no-op. This one IS safe to call from inside RunInTx: it
// goes through tx, which reuses the SAME connection RunInTx already checked
// out for this attempt (see txRepo, postgres.Repository.RunInTx), not a
// second one from the pool.
func enforceTenantPolicy(ctx context.Context, tx domain.Tx, tenantID string, policy domain.TenantPolicy, postings []domain.Posting) error {
	var dailyDebits map[string]int64
	if policy.DailyVolumeLimit > 0 {
		var err error
		dailyDebits, err = tx.TenantDailyDebits(ctx, tenantID)
		if err != nil {
			return err
		}
	}

	return domain.CheckTransactionPolicy(policy, postings, dailyDebits)
}

// tenantPolicy resolves tenantID's TenantPolicy from tenants.settings via
// the plain (non-transactional) Repository.GetTenant. It MUST be called
// before RunInTx opens its transaction, never from inside RunInTx's closure
// (see enforceTenantPolicy's doc comment for the connection-pool deadlock
// that would otherwise cause under a small pool). Settings rarely change, so
// reading it once per post, on its own connection, fully resolved before the
// posting transaction even begins, is acceptable (unlike the daily-volume
// total, there is no write on the other side of this read to race against).
//
// A tenant with no row at all resolves to the zero-value policy (no
// guardrails) rather than propagating domain.ErrTenantNotFound: every real
// caller's tenant already exists by the time it can post (accounts and
// transactions both carry a foreign key to tenants.id, so an account could
// not have been created for it otherwise), so this only ever fires for a
// caller that bypasses that constraint (an in-memory test double, or a
// tenant id typo that will fail some other way inside the transaction
// regardless); failing open here is strictly safer than turning a
// well-formed post into a policy-shaped 422 for an unrelated reason.
func tenantPolicy(ctx context.Context, repo domain.Repository, tenantID string) (domain.TenantPolicy, error) {
	tenant, err := repo.GetTenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, domain.ErrTenantNotFound) {
			return domain.TenantPolicy{}, nil
		}
		return domain.TenantPolicy{}, err
	}
	settings, err := domain.ParseTenantSettings(tenant.Settings)
	if err != nil {
		return domain.TenantPolicy{}, err
	}
	return settings.Policy, nil
}
