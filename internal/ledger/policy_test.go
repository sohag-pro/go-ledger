package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// setTenantPolicy writes tenantID's tenants.settings jsonb directly (Task
// 2.4b, audit A3.4), the same "raw SQL fixture" style seedConvertRate (see
// convert_test.go) uses for fx_rates: this package tests the ledger's
// enforcement of a policy, not the admin surface that sets one (that is
// covered by internal/admin and internal/api's own tests), so it writes the
// settings column directly rather than importing internal/admin.
func setTenantPolicy(t *testing.T, pool *pgxpool.Pool, tenantID string, policy domain.TenantPolicy) {
	t.Helper()
	raw, err := json.Marshal(domain.TenantSettings{Policy: policy})
	if err != nil {
		t.Fatalf("marshal tenant settings: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE tenants SET settings = $1 WHERE id = $2`, raw, tenantID); err != nil {
		t.Fatalf("set tenant policy: %v", err)
	}
}

// newPolicyTenant creates a fresh tenant with two USD accounts and two EUR
// accounts (enough to post a balanced transaction in either currency, for
// both single- and per-currency policy checks), and returns the tenant id
// alongside the accounts.
func newPolicyTenant(t *testing.T, repo *postgres.Repository) (tenant string, usdA, usdB, eurA, eurB domain.Account) {
	t.Helper()
	ctx := context.Background()
	tenant = uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "policy test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	usdA = mustCreateAccount(t, repo, tenant, "USD")
	usdB = mustCreateAccount(t, repo, tenant, "USD")
	eurA = mustCreateAccount(t, repo, tenant, "EUR")
	eurB = mustCreateAccount(t, repo, tenant, "EUR")
	return tenant, usdA, usdB, eurA, eurB
}

func mustCreateAccount(t *testing.T, repo *postgres.Repository, tenant string, currency domain.Currency) domain.Account {
	t.Helper()
	a := &domain.Account{Name: "acct-" + uuid.NewString(), Type: domain.Asset, Currency: currency}
	if err := repo.CreateAccount(context.Background(), tenant, a); err != nil {
		t.Fatalf("create %s account: %v", currency, err)
	}
	return *a
}

// txnOf builds a two-posting transaction moving amount from credit into
// debit, in currency.
func txnOf(t *testing.T, debit, credit string, amount int64, currency domain.Currency) *domain.Transaction {
	t.Helper()
	d, err := domain.NewMoney(amount, currency)
	if err != nil {
		t.Fatalf("NewMoney debit: %v", err)
	}
	c, err := domain.NewMoney(-amount, currency)
	if err != nil {
		t.Fatalf("NewMoney credit: %v", err)
	}
	return &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit, Amount: d},
		{AccountID: credit, Amount: c},
	}}
}

// assertPolicyViolation fails the test unless err is a *domain.PolicyViolationError
// with the given rule.
func assertPolicyViolation(t *testing.T, err error, wantRule domain.PolicyRule) {
	t.Helper()
	var pv *domain.PolicyViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("err = %v, want *domain.PolicyViolationError", err)
	}
	if !errors.Is(err, domain.ErrPolicyViolation) {
		t.Error("err does not match domain.ErrPolicyViolation via errors.Is")
	}
	if pv.Rule != wantRule {
		t.Errorf("PolicyViolationError.Rule = %s, want %s", pv.Rule, wantRule)
	}
}

// TestPost_MaxTransactionAmount_RejectsOverCapPostsUnderCap covers the
// max-transaction-amount guardrail (Task 2.4b, audit A3.4): a transaction
// whose per-currency debit total exceeds the tenant's configured cap is
// rejected with a *domain.PolicyViolationError, and one at or under the cap
// posts normally.
func TestPost_MaxTransactionAmount_RejectsOverCapPostsUnderCap(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, usdA, usdB, _, _ := newPolicyTenant(t, repo)
	setTenantPolicy(t, pool, tenant, domain.TenantPolicy{MaxTransactionAmount: 1000})

	// Over the cap: rejected, and nothing is persisted (the policy check
	// runs before CreateTransaction assigns an id, so t.ID stays empty).
	over := txnOf(t, usdA.ID, usdB.ID, 1001, "USD")
	_, err := svc.Post(ctx, tenant, over, nil)
	assertPolicyViolation(t, err, domain.PolicyRuleMaxTransactionAmount)
	if over.ID != "" {
		t.Errorf("a rejected post was assigned an id: %q", over.ID)
	}

	// At the cap: posts.
	atCap := txnOf(t, usdA.ID, usdB.ID, 1000, "USD")
	if _, err := svc.Post(ctx, tenant, atCap, nil); err != nil {
		t.Errorf("post at cap rejected: %v", err)
	}

	// Under the cap: posts.
	under := txnOf(t, usdA.ID, usdB.ID, 500, "USD")
	if _, err := svc.Post(ctx, tenant, under, nil); err != nil {
		t.Errorf("post under cap rejected: %v", err)
	}
}

// TestPost_AllowedCurrencies_RejectsDisallowedCurrency covers the currency
// allowlist guardrail: a posting in a currency outside a non-empty
// AllowedCurrencies is rejected, while one in an allowed currency posts.
func TestPost_AllowedCurrencies_RejectsDisallowedCurrency(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, usdA, usdB, eurA, eurB := newPolicyTenant(t, repo)
	setTenantPolicy(t, pool, tenant, domain.TenantPolicy{AllowedCurrencies: []string{"USD"}})

	// EUR is not allowed.
	disallowed := txnOf(t, eurA.ID, eurB.ID, 100, "EUR")
	_, err := svc.Post(ctx, tenant, disallowed, nil)
	assertPolicyViolation(t, err, domain.PolicyRuleCurrencyNotAllowed)

	// USD is allowed.
	allowed := txnOf(t, usdA.ID, usdB.ID, 100, "USD")
	if _, err := svc.Post(ctx, tenant, allowed, nil); err != nil {
		t.Errorf("post in allowed currency rejected: %v", err)
	}
}

// TestPost_DailyVolumeLimit_PerCurrency covers the daily-volume guardrail,
// including that it is evaluated PER CURRENCY: posting up to the cap in USD
// must not affect a same-day EUR post, since the two currencies' totals are
// never summed together.
func TestPost_DailyVolumeLimit_PerCurrency(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, usdA, usdB, eurA, eurB := newPolicyTenant(t, repo)
	setTenantPolicy(t, pool, tenant, domain.TenantPolicy{DailyVolumeLimit: 1000})

	// Post 700 USD: well under the 1000 cap.
	first := txnOf(t, usdA.ID, usdB.ID, 700, "USD")
	if _, err := svc.Post(ctx, tenant, first, nil); err != nil {
		t.Fatalf("first post rejected: %v", err)
	}

	// Post 200 more USD: 900 today, still under the cap.
	second := txnOf(t, usdA.ID, usdB.ID, 200, "USD")
	if _, err := svc.Post(ctx, tenant, second, nil); err != nil {
		t.Fatalf("second post rejected: %v", err)
	}

	// Post 200 more USD: would total 1100, over the 1000 cap. Rejected.
	third := txnOf(t, usdA.ID, usdB.ID, 200, "USD")
	_, err := svc.Post(ctx, tenant, third, nil)
	assertPolicyViolation(t, err, domain.PolicyRuleDailyVolumeLimit)

	// A EUR post for the same tenant, same day, is unaffected: EUR has no
	// volume posted yet, and USD's total never counts toward it.
	eurPost := txnOf(t, eurA.ID, eurB.ID, 900, "EUR")
	if _, err := svc.Post(ctx, tenant, eurPost, nil); err != nil {
		t.Errorf("EUR post rejected by USD's daily total: %v", err)
	}

	// 150 more USD would total 1050 (900 + 150): still rejected.
	fourth := txnOf(t, usdA.ID, usdB.ID, 150, "USD")
	_, err = svc.Post(ctx, tenant, fourth, nil)
	assertPolicyViolation(t, err, domain.PolicyRuleDailyVolumeLimit)

	// Exactly 100 more USD reaches the cap precisely (1000): allowed.
	fifth := txnOf(t, usdA.ID, usdB.ID, 100, "USD")
	if _, err := svc.Post(ctx, tenant, fifth, nil); err != nil {
		t.Errorf("post reaching the cap exactly was rejected: %v", err)
	}
}

// TestPost_NoPolicy_Unaffected proves a tenant with no policy configured at
// all posts a large, otherwise-arbitrary transaction exactly as it always
// has: TenantPolicy's zero value must never gate anything.
func TestPost_NoPolicy_Unaffected(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, usdA, usdB, _, _ := newPolicyTenant(t, repo)
	// No setTenantPolicy call: settings stays "{}" all the way from CreateTenant.

	huge := txnOf(t, usdA.ID, usdB.ID, 1_000_000_000, "USD")
	if _, err := svc.Post(ctx, tenant, huge, nil); err != nil {
		t.Errorf("post rejected for a tenant with no policy: %v", err)
	}
}

// newConvertServiceForPolicy is newConvertService (convert_test.go) with a
// different name only for readability at each call site in this file; it is
// the same wiring (a real fx.Provider over pool).
func newConvertServiceForPolicy(pool *pgxpool.Pool) *ledger.TransactionService {
	repo := postgres.NewRepository(pool)
	return ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithFXProvider(fx.NewDBProvider(pool)))
}

// TestConvert_PolicyEnforced proves Convert enforces the same TenantPolicy
// guardrails Post does, over the FULL set of legs it builds (source debit,
// both clearing legs, destination credit): a max-transaction-amount cap set
// on the destination currency rejects a convert whose converted amount
// exceeds it, and a currency allowlist that excludes the destination
// currency rejects the convert too.
func TestConvert_PolicyEnforced(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertServiceForPolicy(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	const (
		base, quote = domain.Currency("USD"), domain.Currency("GBP")
		midE8       = 80_000_000 // 0.80 GBP per USD
		spreadBps   = 0
	)
	seedConvertRate(t, pool, quote, midE8, spreadBps)

	usd := newConvertAccount(t, repo, tenant, base)
	gbp := newConvertAccount(t, repo, tenant, quote)

	t.Run("max transaction amount on destination currency", func(t *testing.T) {
		setTenantPolicy(t, pool, tenant, domain.TenantPolicy{MaxTransactionAmount: 100})
		// $100.00 converts to £80.00 (8000 minor units), over the £100 cap... but
		// to keep this simple and deterministic, cap GBP at a value the
		// converted amount will clearly exceed: 100 minor units (a a little
		// over $1).
		req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: gbp.ID, SourceAmount: 10_000}
		_, _, err := svc.Convert(ctx, tenant, req, &domain.Idempotency{Key: "convert-policy-cap"})
		assertPolicyViolation(t, err, domain.PolicyRuleMaxTransactionAmount)
	})

	t.Run("currency allowlist excludes destination currency", func(t *testing.T) {
		setTenantPolicy(t, pool, tenant, domain.TenantPolicy{AllowedCurrencies: []string{"USD"}})
		req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: gbp.ID, SourceAmount: 10_000}
		_, _, err := svc.Convert(ctx, tenant, req, &domain.Idempotency{Key: "convert-policy-allowlist"})
		assertPolicyViolation(t, err, domain.PolicyRuleCurrencyNotAllowed)
	})

	t.Run("policy cleared allows the convert", func(t *testing.T) {
		setTenantPolicy(t, pool, tenant, domain.TenantPolicy{})
		req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: gbp.ID, SourceAmount: 10_000}
		if _, _, err := svc.Convert(ctx, tenant, req, &domain.Idempotency{Key: "convert-policy-clear"}); err != nil {
			t.Errorf("Convert() with no policy = %v, want nil", err)
		}
	})
}
