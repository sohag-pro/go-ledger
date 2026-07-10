package grpcserver_test

// Tests for the Convert RPC (Task 8, Week 11): REST-parity FX semantics and
// errors over gRPC. These are integration tests against the real Postgres
// repository, the same harness server_test.go and coverage_test.go use, since
// Convert needs a real fx.Provider (internal/fx's Postgres-backed one) to
// resolve a rate, not a fake.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	ledgerv1 "github.com/sohag-pro/go-ledger/internal/genproto/ledger/v1"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// convertClient dials the real gRPC server the same way dialClient does, but
// wires the TransactionService with a real fx.DBProvider backed by sharedPool,
// so Convert has an actual rate source instead of erroring with
// ledger.ErrNoFXProvider.
func convertClient(t *testing.T) ledgerv1.LedgerServiceClient {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	return dialClient(t, ledger.WithFXProvider(fx.NewDBProvider(sharedPool)))
}

// seedFXRate inserts a fresh fx_rates row for USD/quote directly against
// sharedPool, so Convert tests do not depend on any seeded FX_RATES env var.
// Every test below converts from USD, and each uses its own quote currency so
// CurrentFXRate's "most recent row for this pair" lookup never has to
// disambiguate between two tests.
func seedFXRate(t *testing.T, quote string, midRateE8 int64, spreadBps int32) {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	q := sqlc.New(sharedPool)
	if _, err := q.InsertFXRate(context.Background(), sqlc.InsertFXRateParams{
		Base: "USD", Quote: quote, MidRateE8: midRateE8, SpreadBps: spreadBps,
		Source: "test", EffectiveAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed fx rate USD/%s: %v", quote, err)
	}
}

// TestGRPCConvertTransaction covers the Convert RPC end to end: a valid
// conversion returns the four-leg transaction with per-posting currency and
// the FX rate detail, a replay with the same idempotency key returns the
// same transaction, and a later GetTransaction still reports per-posting
// currency (the fx_* snapshot is convert-response-only, per ADR-014).
func TestGRPCConvertTransaction(t *testing.T) {
	client := convertClient(t)
	ctx := authedCtx(context.Background())
	seedFXRate(t, "EUR", 92_000_000, 50)

	usd, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Convert Checking", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create usd: %v", err)
	}
	eur, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Convert Savings EUR", Type: "asset", Currency: "EUR"})
	if err != nil {
		t.Fatalf("create eur: %v", err)
	}

	convCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "grpc-convert-happy-1")
	resp, err := client.Convert(convCtx, &ledgerv1.ConvertRequest{
		FromAccount: usd.Account.Id, ToAccount: eur.Account.Id, SourceAmount: 10000,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if resp.Replayed {
		t.Error("first convert should not be a replay")
	}
	if len(resp.Transaction.Postings) != 4 {
		t.Fatalf("postings = %d, want 4", len(resp.Transaction.Postings))
	}
	var sawUSDLeg, sawEURLeg bool
	for _, p := range resp.Transaction.Postings {
		if p.AccountId == usd.Account.Id && p.Currency == "USD" {
			sawUSDLeg = true
		}
		if p.AccountId == eur.Account.Id && p.Currency == "EUR" {
			sawEURLeg = true
		}
	}
	if !sawUSDLeg || !sawEURLeg {
		t.Errorf("postings = %+v, want a USD leg on %s and a EUR leg on %s", resp.Transaction.Postings, usd.Account.Id, eur.Account.Id)
	}
	if resp.Fx == nil {
		t.Fatal("fx detail is nil")
	}
	if resp.Fx.SourceAmount != 10000 {
		t.Errorf("fx.source_amount = %d, want 10000", resp.Fx.SourceAmount)
	}
	if resp.Fx.MidRateE8 != 92_000_000 || resp.Fx.SpreadBps != 50 {
		t.Errorf("fx = %+v, want mid_rate_e8 92000000 spread_bps 50", resp.Fx)
	}
	if resp.Fx.ConvertedAmount <= 0 {
		t.Errorf("fx.converted_amount = %d, want > 0", resp.Fx.ConvertedAmount)
	}
	if resp.Fx.RateSource != "test" {
		t.Errorf("fx.rate_source = %q, want test", resp.Fx.RateSource)
	}
	if _, err := time.Parse(time.RFC3339Nano, resp.Fx.EffectiveAt); err != nil {
		t.Errorf("fx.effective_at %q is not RFC3339Nano: %v", resp.Fx.EffectiveAt, err)
	}

	// A fetched transaction must report per-posting currency too.
	got, err := client.GetTransaction(ctx, &ledgerv1.GetTransactionRequest{Id: resp.Transaction.Id})
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	sawUSDLeg, sawEURLeg = false, false
	for _, p := range got.Transaction.Postings {
		if p.AccountId == usd.Account.Id && p.Currency == "USD" {
			sawUSDLeg = true
		}
		if p.AccountId == eur.Account.Id && p.Currency == "EUR" {
			sawEURLeg = true
		}
	}
	if !sawUSDLeg || !sawEURLeg {
		t.Errorf("GET postings = %+v, want a USD leg on %s and a EUR leg on %s", got.Transaction.Postings, usd.Account.Id, eur.Account.Id)
	}

	// Replay: same idempotency key returns the same transaction.
	replay, err := client.Convert(convCtx, &ledgerv1.ConvertRequest{
		FromAccount: usd.Account.Id, ToAccount: eur.Account.Id, SourceAmount: 10000,
	})
	if err != nil {
		t.Fatalf("replay convert: %v", err)
	}
	if !replay.Replayed {
		t.Error("second convert with same key should be a replay")
	}
	if replay.Transaction.Id != resp.Transaction.Id {
		t.Errorf("replay id = %s, want %s", replay.Transaction.Id, resp.Transaction.Id)
	}
}

func TestGRPCConvertDust(t *testing.T) {
	client := convertClient(t)
	ctx := authedCtx(context.Background())
	// A mid rate of 1 (1e-8 quote units per base unit) with a source of 1
	// minor unit rounds to zero quote-currency minor units: dust.
	seedFXRate(t, "JPY", 1, 0)

	usd, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Dust USD", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create usd: %v", err)
	}
	jpy, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Dust JPY", Type: "asset", Currency: "JPY"})
	if err != nil {
		t.Fatalf("create jpy: %v", err)
	}

	convCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "grpc-convert-dust")
	_, err = client.Convert(convCtx, &ledgerv1.ConvertRequest{
		FromAccount: usd.Account.Id, ToAccount: jpy.Account.Id, SourceAmount: 1,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCConvertSelfAccount(t *testing.T) {
	client := convertClient(t)
	ctx := authedCtx(context.Background())
	usd, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Self USD", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create usd: %v", err)
	}

	convCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "grpc-convert-self")
	_, err = client.Convert(convCtx, &ledgerv1.ConvertRequest{
		FromAccount: usd.Account.Id, ToAccount: usd.Account.Id, SourceAmount: 100,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCConvertSameCurrency(t *testing.T) {
	client := convertClient(t)
	ctx := authedCtx(context.Background())
	usd1, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "SameCur A", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create usd1: %v", err)
	}
	usd2, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "SameCur B", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create usd2: %v", err)
	}

	convCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "grpc-convert-same-currency")
	_, err = client.Convert(convCtx, &ledgerv1.ConvertRequest{
		FromAccount: usd1.Account.Id, ToAccount: usd2.Account.Id, SourceAmount: 100,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCConvertMissingRatePair(t *testing.T) {
	client := convertClient(t)
	ctx := authedCtx(context.Background())
	// GBP/CHF: deliberately never seeded, in either direction.
	gbp, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "NoRate GBP", Type: "asset", Currency: "GBP"})
	if err != nil {
		t.Fatalf("create gbp: %v", err)
	}
	chf, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "NoRate CHF", Type: "asset", Currency: "CHF"})
	if err != nil {
		t.Fatalf("create chf: %v", err)
	}

	convCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "grpc-convert-no-rate")
	_, err = client.Convert(convCtx, &ledgerv1.ConvertRequest{
		FromAccount: gbp.Account.Id, ToAccount: chf.Account.Id, SourceAmount: 100,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCConvertNonPositiveSourceAmount(t *testing.T) {
	client := convertClient(t)
	ctx := authedCtx(context.Background())
	seedFXRate(t, "CAD", 135_000_000, 0)
	usd, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "NonPositive USD", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create usd: %v", err)
	}
	cad, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "NonPositive CAD", Type: "asset", Currency: "CAD"})
	if err != nil {
		t.Fatalf("create cad: %v", err)
	}

	convCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "grpc-convert-non-positive")
	_, err = client.Convert(convCtx, &ledgerv1.ConvertRequest{
		FromAccount: usd.Account.Id, ToAccount: cad.Account.Id, SourceAmount: 0,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCConvertMissingIdempotencyKey(t *testing.T) {
	client := convertClient(t)
	ctx := authedCtx(context.Background())
	seedFXRate(t, "AUD", 150_000_000, 0)
	usd, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "NoKey USD", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create usd: %v", err)
	}
	aud, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "NoKey AUD", Type: "asset", Currency: "AUD"})
	if err != nil {
		t.Fatalf("create aud: %v", err)
	}

	_, err = client.Convert(ctx, &ledgerv1.ConvertRequest{
		FromAccount: usd.Account.Id, ToAccount: aud.Account.Id, SourceAmount: 100,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCConvertUnauthenticated(t *testing.T) {
	client := convertClient(t)
	_, err := client.Convert(context.Background(), &ledgerv1.ConvertRequest{
		FromAccount: "x", ToAccount: "y", SourceAmount: 100,
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
}

// TestGRPCConvertCrossTenantIsolation checks that Convert rejects a to_account
// belonging to a different tenant with NotFound, mirroring
// TestConvertCrossTenantIsolation_Postgres in internal/api/auth_test.go. This
// needs the real Postgres repository (not a fake), since only it enforces
// tenant scoping (ADR-012).
func TestGRPCConvertCrossTenantIsolation(t *testing.T) {
	client := convertClient(t)
	repo := postgres.NewRepository(sharedPool)
	ctx := context.Background()

	seedFXRate(t, "PLN", 100_000_000, 0)

	tenantB := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenantB, "tenant B"); err != nil {
		t.Fatalf("create tenant B: %v", err)
	}
	plnB := &domain.Account{Name: "Tenant B PLN", Type: domain.Asset, Currency: "PLN"}
	if err := repo.CreateAccount(ctx, tenantB, plnB); err != nil {
		t.Fatalf("create tenant B account: %v", err)
	}

	usdA, err := client.CreateAccount(authedCtx(ctx), &ledgerv1.CreateAccountRequest{Name: "Tenant A USD", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create tenant A account: %v", err)
	}

	convCtx := metadata.AppendToOutgoingContext(authedCtx(ctx), "idempotency-key", "grpc-convert-cross-tenant")
	_, err = client.Convert(convCtx, &ledgerv1.ConvertRequest{
		FromAccount: usdA.Account.Id, ToAccount: plnB.ID, SourceAmount: 100,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("convert to another tenant's account code = %v, want NotFound", status.Code(err))
	}
}
