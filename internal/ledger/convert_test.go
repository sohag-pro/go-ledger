package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// seedConvertRate writes one fx_rates row directly via sqlc, bypassing the
// fx package's own env-seeding path, exactly the way internal/fx's own tests
// set up fixtures. fx_rates is global (not tenant-scoped), so every test in
// this PACKAGE (not just this file: see also convert_roundtrip_test.go) uses
// its own quote currency to avoid the "current rate" resolving to a row a
// different test appended. Every test in this file converts FROM USD, so base
// is fixed rather than threaded through as a parameter that never varies.
func seedConvertRate(t *testing.T, pool *pgxpool.Pool, quote domain.Currency, midE8 int64, spreadBps int32) {
	t.Helper()
	q := sqlc.New(pool)
	if _, err := q.InsertFXRate(context.Background(), sqlc.InsertFXRateParams{
		Base:        "USD",
		Quote:       string(quote),
		MidRateE8:   midE8,
		SpreadBps:   spreadBps,
		Source:      "test",
		EffectiveAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed fx rate USD/%s: %v", quote, err)
	}
}

// seedTenantConvertRate writes one tenant-scoped fx_rates row directly via
// sqlc (Task 2.4, audit A3.3), the tenant-scoped counterpart of
// seedConvertRate above: tenantID must already have a tenants row (see
// newConvertAccount, which creates one), since fx_rates.tenant_id carries a
// foreign key to tenants.id.
func seedTenantConvertRate(t *testing.T, pool *pgxpool.Pool, tenantID string, base, quote domain.Currency, midE8 int64, spreadBps int32) {
	t.Helper()
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		t.Fatalf("parse tenant id %q: %v", tenantID, err)
	}
	q := sqlc.New(pool)
	if _, err := q.InsertFXRate(context.Background(), sqlc.InsertFXRateParams{
		TenantID:    pgtype.UUID{Bytes: tid, Valid: true},
		Base:        string(base),
		Quote:       string(quote),
		MidRateE8:   midE8,
		SpreadBps:   spreadBps,
		Source:      "test-tenant",
		EffectiveAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed tenant fx rate %s/%s for tenant %s: %v", base, quote, tenantID, err)
	}
}

// newConvertAccount creates and returns an account of the given currency. It
// ensures tenant's own row exists first (accounts_tenant_fk, migration 0011):
// every test in this file calls this for a freshly generated tenant id with
// no tenant row of its own, and a caller that creates more than one account
// for the same tenant is unaffected (ErrTenantAlreadyExists on the second
// call is swallowed).
func newConvertAccount(t *testing.T, repo *postgres.Repository, tenant string, currency domain.Currency) domain.Account {
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, tenant, "convert test tenant"); err != nil && !errors.Is(err, domain.ErrTenantAlreadyExists) {
		t.Fatalf("create tenant: %v", err)
	}
	a := &domain.Account{Name: "acct-" + uuid.NewString(), Type: domain.Asset, Currency: currency}
	if err := repo.CreateAccount(ctx, tenant, a); err != nil {
		t.Fatalf("create %s account: %v", currency, err)
	}
	return *a
}

// newConvertService returns a TransactionService wired with a real fx.Provider
// over pool, the only way Convert is reachable (see ErrNoFXProvider).
func newConvertService(pool *pgxpool.Pool) *ledger.TransactionService {
	repo := postgres.NewRepository(pool)
	return ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithFXProvider(fx.NewDBProvider(pool)))
}

// TestConvert_BalancesPerCurrencyAndRecordsRate is Step 1 of Task 6: convert
// USD to EUR and check every hard-rule guarantee in one pass: four postings,
// each currency nets to zero on its own, the clearing accounts hold the open
// position, the transaction row carries the FX snapshot, and the audit row
// records per-posting currency plus the rate detail.
func TestConvert_BalancesPerCurrencyAndRecordsRate(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	const (
		base, quote = domain.Currency("USD"), domain.Currency("EUR")
		midE8       = 92_000_000 // 0.92 EUR per USD
		spreadBps   = 50
	)
	seedConvertRate(t, pool, quote, midE8, spreadBps)

	usd := newConvertAccount(t, repo, tenant, base)
	eur := newConvertAccount(t, repo, tenant, quote)

	const sourceAmount = 10_000 // $100.00
	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: eur.ID, SourceAmount: sourceAmount}
	idem := &domain.Idempotency{Key: "convert-balances-1"}

	txn, replayed, err := svc.Convert(ctx, tenant, req, idem)
	if err != nil {
		t.Fatalf("Convert() error = %v", err)
	}
	if replayed {
		t.Fatalf("Convert() replayed = true on first call, want false")
	}
	if len(txn.Postings) != 4 {
		t.Fatalf("Convert() postings = %d, want 4", len(txn.Postings))
	}

	// Each currency must net to zero on its own (ADR-014's per-currency
	// invariant), and this is exactly what Transaction.Validate already
	// enforced before the write, so this check is really about the postings
	// having landed with the right signs and currencies in storage.
	sums := map[domain.Currency]int64{}
	for _, p := range txn.Postings {
		sums[p.Amount.Currency()] += p.Amount.Amount()
	}
	if sums[base] != 0 {
		t.Errorf("USD postings sum = %d, want 0", sums[base])
	}
	if sums[quote] != 0 {
		t.Errorf("EUR postings sum = %d, want 0", sums[quote])
	}

	// Cross-check the converted amount against the same formula the service
	// calls internally (domain.Convert), with the exact mid/spread seeded
	// above: this is verifying the service plumbed the seeded rate through
	// correctly, not re-deriving the rounding rule (that is unit tested in
	// internal/domain).
	source, err := domain.NewMoney(sourceAmount, base)
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	wantConverted, wantAppliedE8, err := domain.Convert(source, quote, midE8, spreadBps)
	if err != nil {
		t.Fatalf("domain.Convert: %v", err)
	}
	if txn.FX == nil {
		t.Fatalf("Convert() transaction has no FX detail")
	}
	if txn.FX.SourceAmount != sourceAmount {
		t.Errorf("FX.SourceAmount = %d, want %d", txn.FX.SourceAmount, sourceAmount)
	}
	if txn.FX.ConvertedAmount != wantConverted.Amount() {
		t.Errorf("FX.ConvertedAmount = %d, want %d", txn.FX.ConvertedAmount, wantConverted.Amount())
	}
	if txn.FX.MidRateE8 != midE8 {
		t.Errorf("FX.MidRateE8 = %d, want %d", txn.FX.MidRateE8, midE8)
	}
	if txn.FX.SpreadBps != spreadBps {
		t.Errorf("FX.SpreadBps = %d, want %d", txn.FX.SpreadBps, spreadBps)
	}
	if txn.FX.AppliedE8 != wantAppliedE8 {
		t.Errorf("FX.AppliedE8 = %d, want %d", txn.FX.AppliedE8, wantAppliedE8)
	}
	if txn.FX.RateSource != "test" {
		t.Errorf("FX.RateSource = %q, want %q", txn.FX.RateSource, "test")
	}
	if txn.FX.RateID == 0 {
		t.Errorf("FX.RateID = 0, want a real fx_rates row id")
	}

	// The transaction row itself must carry the same snapshot when re-read
	// from storage: this is Task 6's fx_* column wiring on GetTransaction.
	stored, err := repo.GetTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if stored.FX == nil {
		t.Fatalf("stored transaction has no FX snapshot")
	}
	if *stored.FX != *txn.FX {
		t.Errorf("stored FX = %+v, want %+v", *stored.FX, *txn.FX)
	}
	// Every posting must carry its own currency on re-read (ADR-014): USD
	// legs read back as USD, EUR legs as EUR.
	for _, p := range stored.Postings {
		if p.AccountID == usd.ID && p.Amount.Currency() != base {
			t.Errorf("posting on USD account has currency %s, want USD", p.Amount.Currency())
		}
		if p.AccountID == eur.ID && p.Amount.Currency() != quote {
			t.Errorf("posting on EUR account has currency %s, want EUR", p.Amount.Currency())
		}
	}

	// The clearing accounts hold the open position: clearing_USD received the
	// source amount (a debit-normal Asset account's +10000 mirrors the user's
	// -10000), clearing_EUR gave up the converted amount.
	clearingUSD, err := repo.GetOrCreateClearingAccount(ctx, tenant, base)
	if err != nil {
		t.Fatalf("get clearing USD: %v", err)
	}
	clearingEUR, err := repo.GetOrCreateClearingAccount(ctx, tenant, quote)
	if err != nil {
		t.Fatalf("get clearing EUR: %v", err)
	}
	if !clearingUSD.System {
		t.Errorf("clearing USD account System = false, want true")
	}
	usdBal, err := repo.Balance(ctx, tenant, clearingUSD.ID)
	if err != nil {
		t.Fatalf("balance clearing USD: %v", err)
	}
	if usdBal.Amount() != sourceAmount {
		t.Errorf("clearing USD balance = %d, want %d", usdBal.Amount(), sourceAmount)
	}
	eurBal, err := repo.Balance(ctx, tenant, clearingEUR.ID)
	if err != nil {
		t.Fatalf("balance clearing EUR: %v", err)
	}
	if eurBal.Amount() != -wantConverted.Amount() {
		t.Errorf("clearing EUR balance = %d, want %d", eurBal.Amount(), -wantConverted.Amount())
	}

	// The audit row must record per-posting currency and the rate detail, not
	// one top-level currency stamped from postings[0] (the pre-Task-6 shape).
	audit, err := repo.ListAuditByTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(audit) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit))
	}
	var snapshot struct {
		Postings []struct {
			AccountID string `json:"account_id"`
			Currency  string `json:"currency"`
		} `json:"postings"`
		FX struct {
			MidRateE8  int64  `json:"mid_rate_e8"`
			SpreadBps  int32  `json:"spread_bps"`
			RateSource string `json:"rate_source"`
		} `json:"fx"`
	}
	if err := json.Unmarshal(audit[0].After, &snapshot); err != nil {
		t.Fatalf("unmarshal audit snapshot: %v", err)
	}
	if len(snapshot.Postings) != 4 {
		t.Fatalf("audit snapshot postings = %d, want 4", len(snapshot.Postings))
	}
	seenUSD, seenEUR := false, false
	for _, p := range snapshot.Postings {
		switch p.AccountID {
		case usd.ID:
			seenUSD = p.Currency == "USD"
		case eur.ID:
			seenEUR = p.Currency == "EUR"
		}
	}
	if !seenUSD {
		t.Errorf("audit snapshot: USD posting missing or not stamped currency USD")
	}
	if !seenEUR {
		t.Errorf("audit snapshot: EUR posting missing or not stamped currency EUR")
	}
	if snapshot.FX.MidRateE8 != midE8 || snapshot.FX.SpreadBps != spreadBps || snapshot.FX.RateSource != "test" {
		t.Errorf("audit snapshot fx = %+v, want mid %d spread %d source test", snapshot.FX, midE8, spreadBps)
	}
}

// TestConvert_IdempotentRetryReplaysDespiteRateMove is the hard-rule test:
// resolving idempotency from the request, before the rate lookup, means a
// retry with the same key replays the original conversion even after a new
// (later) rate has been appended for the same pair, instead of recomputing a
// different converted amount and 409ing on a postings-based fingerprint.
func TestConvert_IdempotentRetryReplaysDespiteRateMove(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	const base, quote = domain.Currency("USD"), domain.Currency("GBP")
	seedConvertRate(t, pool, quote, 80_000_000, 0) // 0.80 GBP per USD

	usd := newConvertAccount(t, repo, tenant, base)
	gbp := newConvertAccount(t, repo, tenant, quote)

	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: gbp.ID, SourceAmount: 5_000}
	idem := &domain.Idempotency{Key: "convert-retry-1"}

	first, replayed, err := svc.Convert(ctx, tenant, req, idem)
	if err != nil {
		t.Fatalf("first Convert() error = %v", err)
	}
	if replayed {
		t.Fatalf("first Convert() replayed = true, want false")
	}

	// The rate moves: a new, later fx_rates row for the same pair.
	seedConvertRate(t, pool, quote, 95_000_000, 0) // now 0.95 GBP per USD

	second, replayed, err := svc.Convert(ctx, tenant, req, idem)
	if err != nil {
		t.Fatalf("retry Convert() error = %v", err)
	}
	if !replayed {
		t.Fatalf("retry Convert() replayed = false, want true")
	}
	if second.ID != first.ID {
		t.Errorf("retry transaction id = %s, want %s (the original)", second.ID, first.ID)
	}
	if second.FX == nil || second.FX.MidRateE8 != 80_000_000 {
		t.Errorf("retry FX.MidRateE8 = %+v, want the ORIGINAL 80000000, not the moved rate", second.FX)
	}

	// Exactly one transaction, one audit row: the retry did not re-convert.
	audit, err := repo.ListAuditByTransaction(ctx, tenant, first.ID)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(audit) != 1 {
		t.Errorf("audit rows for the transaction = %d, want 1 (no re-conversion on replay)", len(audit))
	}
}

// TestConvert_IdempotencyConflict checks that reusing a key with a genuinely
// different request (a different source amount) is rejected as a conflict,
// not silently replayed.
func TestConvert_IdempotencyConflict(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	const base, quote = domain.Currency("USD"), domain.Currency("CHF")
	seedConvertRate(t, pool, quote, 90_000_000, 0)

	usd := newConvertAccount(t, repo, tenant, base)
	chf := newConvertAccount(t, repo, tenant, quote)

	idem := &domain.Idempotency{Key: "convert-conflict-1"}
	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: chf.ID, SourceAmount: 1_000}
	if _, _, err := svc.Convert(ctx, tenant, req, idem); err != nil {
		t.Fatalf("first Convert() error = %v", err)
	}

	other := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: chf.ID, SourceAmount: 2_000}
	if _, _, err := svc.Convert(ctx, tenant, other, idem); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("Convert() with reused key, different amount: err = %v, want ErrIdempotencyConflict", err)
	}
}

// TestConvert_RejectsInvalidRequests covers the validation and self/currency
// guards a convert must reject before ever touching the rate provider.
func TestConvert_RejectsInvalidRequests(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()

	t.Run("zero source amount", func(t *testing.T) {
		t.Parallel()
		tenant := uuid.NewString()
		seedConvertRate(t, pool, "SEK", 100_000_000, 0)
		usd := newConvertAccount(t, repo, tenant, "USD")
		sek := newConvertAccount(t, repo, tenant, "SEK")
		req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: sek.ID, SourceAmount: 0}
		if _, _, err := svc.Convert(ctx, tenant, req, nil); !errors.Is(err, domain.ErrNonPositiveConvertAmount) {
			t.Errorf("zero source: err = %v, want ErrNonPositiveConvertAmount", err)
		}
	})

	t.Run("negative source amount", func(t *testing.T) {
		t.Parallel()
		tenant := uuid.NewString()
		seedConvertRate(t, pool, "NOK", 100_000_000, 0)
		usd := newConvertAccount(t, repo, tenant, "USD")
		nok := newConvertAccount(t, repo, tenant, "NOK")
		req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: nok.ID, SourceAmount: -500}
		if _, _, err := svc.Convert(ctx, tenant, req, nil); !errors.Is(err, domain.ErrNonPositiveConvertAmount) {
			t.Errorf("negative source: err = %v, want ErrNonPositiveConvertAmount", err)
		}
	})

	t.Run("self account", func(t *testing.T) {
		t.Parallel()
		tenant := uuid.NewString()
		usd := newConvertAccount(t, repo, tenant, "USD")
		req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: usd.ID, SourceAmount: 100}
		if _, _, err := svc.Convert(ctx, tenant, req, nil); !errors.Is(err, domain.ErrSelfConversion) {
			t.Errorf("self account: err = %v, want ErrSelfConversion", err)
		}
	})

	t.Run("same currency", func(t *testing.T) {
		t.Parallel()
		tenant := uuid.NewString()
		usd1 := newConvertAccount(t, repo, tenant, "USD")
		usd2 := newConvertAccount(t, repo, tenant, "USD")
		req := ledger.ConvertRequest{FromAccountID: usd1.ID, ToAccountID: usd2.ID, SourceAmount: 100}
		if _, _, err := svc.Convert(ctx, tenant, req, nil); !errors.Is(err, domain.ErrSameCurrencyConversion) {
			t.Errorf("same currency: err = %v, want ErrSameCurrencyConversion", err)
		}
	})

	t.Run("dust", func(t *testing.T) {
		t.Parallel()
		tenant := uuid.NewString()
		// A mid rate of 1 (1e-8 quote units per base unit) with a source of 1
		// minor unit rounds to zero quote-currency minor units: dust.
		seedConvertRate(t, pool, "DKK", 1, 0)
		usd := newConvertAccount(t, repo, tenant, "USD")
		dkk := newConvertAccount(t, repo, tenant, "DKK")
		req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: dkk.ID, SourceAmount: 1}
		if _, _, err := svc.Convert(ctx, tenant, req, nil); !errors.Is(err, domain.ErrConversionDust) {
			t.Errorf("dust: err = %v, want ErrConversionDust", err)
		}
	})

	t.Run("cross tenant", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.NewString(), uuid.NewString()
		seedConvertRate(t, pool, "PLN", 100_000_000, 0)
		usdA := newConvertAccount(t, repo, tenantA, "USD")
		plnB := newConvertAccount(t, repo, tenantB, "PLN")
		req := ledger.ConvertRequest{FromAccountID: usdA.ID, ToAccountID: plnB.ID, SourceAmount: 100}
		if _, _, err := svc.Convert(ctx, tenantA, req, nil); !errors.Is(err, domain.ErrAccountNotFound) {
			t.Errorf("cross tenant: err = %v, want ErrAccountNotFound", err)
		}
	})

	// Mirrors "cross tenant" above but on the FROM side: an id that was never
	// created at all, rather than one that belongs to a different tenant.
	// Cross tenant already covers the to-account lookup failing; this covers
	// the from-account lookup failing, the branch immediately before it.
	t.Run("unknown from account", func(t *testing.T) {
		t.Parallel()
		tenant := uuid.NewString()
		seedConvertRate(t, pool, "ZAR", 100_000_000, 0)
		zar := newConvertAccount(t, repo, tenant, "ZAR")
		req := ledger.ConvertRequest{FromAccountID: uuid.NewString(), ToAccountID: zar.ID, SourceAmount: 100}
		if _, _, err := svc.Convert(ctx, tenant, req, nil); !errors.Is(err, domain.ErrAccountNotFound) {
			t.Errorf("unknown from account: err = %v, want ErrAccountNotFound", err)
		}
	})
}

// TestConvert_NoFXProvider checks the construction-time guard: a
// TransactionService built without WithFXProvider reports a clear error
// instead of panicking on a nil interface call.
func TestConvert_NoFXProvider(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil) // no WithFXProvider
	ctx := context.Background()
	tenant := uuid.NewString()

	usd := newConvertAccount(t, repo, tenant, "USD")
	eur := newConvertAccount(t, repo, tenant, "EUR")
	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: eur.ID, SourceAmount: 100}
	if _, _, err := svc.Convert(ctx, tenant, req, nil); !errors.Is(err, ledger.ErrNoFXProvider) {
		t.Errorf("Convert() without a provider: err = %v, want ErrNoFXProvider", err)
	}
}

// TestConvert_NoRateForPair covers the rate-lookup error branch: AUD has no
// fx_rates row in either direction (no USD/AUD, no AUD/USD), so fx.Provider's
// db-backed Rate must surface domain.ErrFXRateNotFound rather than Convert
// panicking or silently defaulting a rate.
func TestConvert_NoRateForPair(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	usd := newConvertAccount(t, repo, tenant, "USD")
	aud := newConvertAccount(t, repo, tenant, "AUD")
	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: aud.ID, SourceAmount: 100}
	if _, _, err := svc.Convert(ctx, tenant, req, nil); !errors.Is(err, domain.ErrFXRateNotFound) {
		t.Errorf("no rate for pair: err = %v, want ErrFXRateNotFound", err)
	}
}

// TestConvert_TenantSpecificSpreadChangesConvertedAmount is the discriminating
// Convert-level test for Task 2.4 (audit A3.3): tenant A gets its own
// USD/MXN row (a different mid AND a wider spread than the global default);
// tenant B has no row of its own. Converting the exact same source amount for
// both tenants must produce two different converted amounts, proving the
// per-tenant rate and spread flow all the way from fx_rates through
// Provider.Rate, domain.Convert, and into the posted transaction's FX
// snapshot, not just through an isolated Rate() lookup.
func TestConvert_TenantSpecificSpreadChangesConvertedAmount(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()

	const (
		base, quote  = domain.Currency("USD"), domain.Currency("MXN")
		globalMidE8  = 1_700_000_000 // 17.00 MXN per USD, an arbitrary fixture rate
		globalSpread = 25
		tenantMidE8  = 1_800_000_000 // 18.00 MXN per USD: tenant A negotiated a different mid too
		tenantSpread = 300           // a much wider spread than the global default
		sourceAmount = 10_000        // $100.00
	)
	seedConvertRate(t, pool, quote, globalMidE8, globalSpread)

	usdA := newConvertAccount(t, repo, tenantA, base)
	mxnA := newConvertAccount(t, repo, tenantA, quote)
	usdB := newConvertAccount(t, repo, tenantB, base)
	mxnB := newConvertAccount(t, repo, tenantB, quote)

	seedTenantConvertRate(t, pool, tenantA, base, quote, tenantMidE8, tenantSpread)

	reqA := ledger.ConvertRequest{FromAccountID: usdA.ID, ToAccountID: mxnA.ID, SourceAmount: sourceAmount}
	txnA, _, err := svc.Convert(ctx, tenantA, reqA, &domain.Idempotency{Key: "tenant-a-rate"})
	if err != nil {
		t.Fatalf("Convert() tenant A error = %v", err)
	}

	reqB := ledger.ConvertRequest{FromAccountID: usdB.ID, ToAccountID: mxnB.ID, SourceAmount: sourceAmount}
	txnB, _, err := svc.Convert(ctx, tenantB, reqB, &domain.Idempotency{Key: "tenant-b-rate"})
	if err != nil {
		t.Fatalf("Convert() tenant B error = %v", err)
	}

	if txnA.FX == nil || txnB.FX == nil {
		t.Fatalf("expected FX detail on both transactions")
	}
	if txnA.FX.MidRateE8 != tenantMidE8 || txnA.FX.SpreadBps != tenantSpread {
		t.Errorf("tenant A FX = {mid: %d, spread: %d}, want {mid: %d, spread: %d} (its own row)",
			txnA.FX.MidRateE8, txnA.FX.SpreadBps, tenantMidE8, tenantSpread)
	}
	if txnB.FX.MidRateE8 != globalMidE8 || txnB.FX.SpreadBps != globalSpread {
		t.Errorf("tenant B FX = {mid: %d, spread: %d}, want {mid: %d, spread: %d} (the global default)",
			txnB.FX.MidRateE8, txnB.FX.SpreadBps, globalMidE8, globalSpread)
	}
	if txnA.FX.ConvertedAmount == txnB.FX.ConvertedAmount {
		t.Errorf("tenant A and tenant B converted the same source amount (%d) to the same result (%d): "+
			"the per-tenant rate/spread did not change the outcome", sourceAmount, txnA.FX.ConvertedAmount)
	}

	// Cross-check against domain.Convert directly with each tenant's own
	// (mid, spread), so this is verifying the service plumbed each tenant's
	// OWN rate through end to end, not just that the two amounts happen to
	// differ for some other reason.
	source, err := domain.NewMoney(sourceAmount, base)
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	wantA, _, err := domain.Convert(source, quote, tenantMidE8, tenantSpread)
	if err != nil {
		t.Fatalf("domain.Convert (tenant A): %v", err)
	}
	wantB, _, err := domain.Convert(source, quote, globalMidE8, globalSpread)
	if err != nil {
		t.Fatalf("domain.Convert (tenant B): %v", err)
	}
	if txnA.FX.ConvertedAmount != wantA.Amount() {
		t.Errorf("tenant A ConvertedAmount = %d, want %d", txnA.FX.ConvertedAmount, wantA.Amount())
	}
	if txnB.FX.ConvertedAmount != wantB.Amount() {
		t.Errorf("tenant B ConvertedAmount = %d, want %d", txnB.FX.ConvertedAmount, wantB.Amount())
	}
}

// TestConvert_ConcurrentIdempotentHammer fires many concurrent Convert calls
// at the same tenant with the same idempotency key. The idempotency precheck
// (GetIdempotencyKey) runs before RunInTx's per-tenant mutex, so more than one
// goroutine can miss the precheck and proceed toward a real conversion; only
// one of those wins the DB's unique constraint on (tenant, key), and every
// other one must observe ErrDuplicateIdempotencyKey inside RunInTx and replay
// the winner's transaction rather than posting a second conversion. This is
// the same hammer pattern TestPostIdempotentHammer uses for Post, applied to
// Convert's separate idempotency and RunInTx wiring.
func TestConvert_ConcurrentIdempotentHammer(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	seedConvertRate(t, pool, "JPY", 150_000_000, 0)
	usd := newConvertAccount(t, repo, tenant, "USD")
	jpy := newConvertAccount(t, repo, tenant, "JPY")

	const n = 25
	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: jpy.ID, SourceAmount: 1_000}
	idem := &domain.Idempotency{Key: "convert-hammer-1"}

	var wg sync.WaitGroup
	ids := make([]string, n)
	replays := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			txn, replayed, err := svc.Convert(ctx, tenant, req, idem)
			if txn != nil {
				ids[i] = txn.ID
			}
			replays[i], errs[i] = replayed, err
		}(i)
	}
	wg.Wait()

	var first string
	replayCount := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if first == "" {
			first = ids[i]
		} else if ids[i] != first {
			t.Fatalf("call %d returned id %s, want %s", i, ids[i], first)
		}
		if replays[i] {
			replayCount++
		}
	}
	if replayCount != n-1 {
		t.Errorf("replay count = %d, want %d (exactly one real conversion)", replayCount, n-1)
	}

	// Exactly one audit row for the one conversion, even under concurrency.
	audit, err := repo.ListAuditByTransaction(ctx, tenant, first)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(audit) != 1 {
		t.Errorf("audit rows = %d, want 1 (no re-conversion under concurrency)", len(audit))
	}
}
