package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestCreateAccountWithPartyFields covers Task 6.1 (audit A9.1): an account
// created with PartyReference and PartyType round-trips through both
// CreateAccount's own return value and a fresh GetAccount read.
func TestCreateAccountWithPartyFields(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "account party reference test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	ref, typ := "cust-12345", "individual"
	acct := &domain.Account{Name: "Checking", Type: domain.Asset, Currency: "USD", PartyReference: &ref, PartyType: &typ}
	if err := repo.CreateAccount(ctx, tenant, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if acct.PartyReference == nil || *acct.PartyReference != ref {
		t.Errorf("CreateAccount party_reference = %v, want %q", acct.PartyReference, ref)
	}
	if acct.PartyType == nil || *acct.PartyType != typ {
		t.Errorf("CreateAccount party_type = %v, want %q", acct.PartyType, typ)
	}

	got, err := repo.GetAccount(ctx, tenant, acct.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.PartyReference == nil || *got.PartyReference != ref {
		t.Errorf("GetAccount party_reference = %v, want %q", got.PartyReference, ref)
	}
	if got.PartyType == nil || *got.PartyType != typ {
		t.Errorf("GetAccount party_type = %v, want %q", got.PartyType, typ)
	}

	list, err := repo.ListAccounts(ctx, tenant, 10)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListAccounts len = %d, want 1", len(list))
	}
	if list[0].PartyReference == nil || *list[0].PartyReference != ref {
		t.Errorf("ListAccounts party_reference = %v, want %q", list[0].PartyReference, ref)
	}
	if list[0].PartyType == nil || *list[0].PartyType != typ {
		t.Errorf("ListAccounts party_type = %v, want %q", list[0].PartyType, typ)
	}
}

// TestCreateAccountWithoutPartyFields covers the nullable default (Task 6.1,
// audit A9.1): an account created without PartyReference/PartyType comes
// back with both nil, exactly as every account behaved before these fields
// existed.
func TestCreateAccountWithoutPartyFields(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "account no party reference test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	acct := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if acct.PartyReference != nil {
		t.Errorf("CreateAccount party_reference = %v, want nil", acct.PartyReference)
	}
	if acct.PartyType != nil {
		t.Errorf("CreateAccount party_type = %v, want nil", acct.PartyType)
	}

	got, err := repo.GetAccount(ctx, tenant, acct.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.PartyReference != nil {
		t.Errorf("GetAccount party_reference = %v, want nil", got.PartyReference)
	}
	if got.PartyType != nil {
		t.Errorf("GetAccount party_type = %v, want nil", got.PartyType)
	}
}
