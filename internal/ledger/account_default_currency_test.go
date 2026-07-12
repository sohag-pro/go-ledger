package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestAccountService_Create_DefaultCurrency table-drives the WithDefaultCurrency
// branch in AccountService.Create (ADR-014, "New-account default currency is
// env-configured"): an empty request currency is stamped with the configured
// default before validation, an explicit request currency is left alone (the
// default is never consulted), and with no default configured an empty
// request currency still falls through to domain.Account.Validate's
// ErrInvalidCurrency, exactly as it did before WithDefaultCurrency existed.
func TestAccountService_Create_DefaultCurrency(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	tests := []struct {
		name            string
		defaultCurrency domain.Currency // "" means WithDefaultCurrency is not applied
		reqCurrency     domain.Currency
		wantCurrency    domain.Currency
		wantErr         error
	}{
		{
			name:            "empty request currency gets the configured default",
			defaultCurrency: "EUR",
			reqCurrency:     "",
			wantCurrency:    "EUR",
		},
		{
			name:            "explicit request currency is used, default ignored",
			defaultCurrency: "EUR",
			reqCurrency:     "GBP",
			wantCurrency:    "GBP",
		},
		{
			name:            "empty request currency with no default configured fails validation",
			defaultCurrency: "",
			reqCurrency:     "",
			wantErr:         domain.ErrInvalidCurrency,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var opts []ledger.AccountOption
			if tt.defaultCurrency != "" {
				opts = append(opts, ledger.WithDefaultCurrency(tt.defaultCurrency))
			}
			svc := ledger.NewAccountService(repo, opts...)
			tenant := uuid.NewString()
			if err := repo.CreateTenant(ctx, tenant, "default currency test tenant"); err != nil {
				t.Fatalf("create tenant: %v", err)
			}

			acct := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: tt.reqCurrency}
			err := svc.Create(ctx, tenant, acct, nil)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("create: got err %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if acct.Currency != tt.wantCurrency {
				t.Errorf("account currency = %q, want %q", acct.Currency, tt.wantCurrency)
			}

			got, err := svc.Get(ctx, tenant, acct.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Currency != tt.wantCurrency {
				t.Errorf("persisted account currency = %q, want %q", got.Currency, tt.wantCurrency)
			}
		})
	}
}
