package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestCreateWebhookSubscriptionAssignsIDAndActive proves CreateWebhookSubscription
// assigns an id when sub.ID is empty, writes it back, and always creates an
// active row (Task 4.1, audit A7.1), and that a listed subscription never
// carries the secret it was created with: domain.WebhookSubscription has no
// field capable of holding one, so this really just proves the metadata
// itself round-trips.
func TestCreateWebhookSubscriptionAssignsIDAndActive(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "webhook create test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	sub := domain.WebhookSubscription{
		TenantID:   tenant,
		URL:        "https://example.com/hooks",
		EventTypes: []string{domain.ActionTransactionCreated},
	}
	if err := repo.CreateWebhookSubscription(ctx, &sub, "whsec_test-secret"); err != nil {
		t.Fatalf("create webhook subscription: %v", err)
	}
	if sub.ID == "" {
		t.Fatal("expected an assigned subscription id")
	}
	if !sub.Active {
		t.Error("expected a newly created subscription to be active")
	}

	subs, err := repo.ListWebhookSubscriptionsByTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("list webhook subscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("ListWebhookSubscriptionsByTenant returned %d subscriptions, want 1", len(subs))
	}
	got := subs[0]
	if got.ID != sub.ID {
		t.Errorf("ID = %q, want %q", got.ID, sub.ID)
	}
	if got.URL != sub.URL {
		t.Errorf("URL = %q, want %q", got.URL, sub.URL)
	}
	if len(got.EventTypes) != 1 || got.EventTypes[0] != domain.ActionTransactionCreated {
		t.Errorf("EventTypes = %v, want [%s]", got.EventTypes, domain.ActionTransactionCreated)
	}
	if !got.Active {
		t.Error("listed subscription Active = false, want true")
	}
	if got.CreatedAt.IsZero() {
		t.Error("listed subscription CreatedAt is zero, want a real timestamp")
	}
}

// TestCreateWebhookSubscriptionMissingTenantErrors proves the tenant-existence
// gate: creating a subscription for a tenant id with no row fails closed with
// domain.ErrTenantNotFound, before any row is written.
func TestCreateWebhookSubscriptionMissingTenantErrors(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	sub := domain.WebhookSubscription{TenantID: uuid.NewString(), URL: "https://example.com/hooks"}
	err := repo.CreateWebhookSubscription(ctx, &sub, "whsec_test-secret")
	if !errors.Is(err, domain.ErrTenantNotFound) {
		t.Errorf("create webhook subscription for missing tenant: err = %v, want ErrTenantNotFound", err)
	}
}

// TestSetWebhookSubscriptionActiveTogglesAndErrorsOnUnknownID proves
// SetWebhookSubscriptionActive flips the active flag both ways and returns
// domain.ErrWebhookSubscriptionNotFound for an id that has no row.
func TestSetWebhookSubscriptionActiveTogglesAndErrorsOnUnknownID(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "webhook set-active test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	sub := domain.WebhookSubscription{TenantID: tenant, URL: "https://example.com/hooks"}
	if err := repo.CreateWebhookSubscription(ctx, &sub, "whsec_test-secret"); err != nil {
		t.Fatalf("create webhook subscription: %v", err)
	}

	if err := repo.SetWebhookSubscriptionActive(ctx, sub.ID, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	subs, err := repo.ListWebhookSubscriptionsByTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 || subs[0].Active {
		t.Fatalf("after deactivate, Active = %v, want false", subs[0].Active)
	}

	if err := repo.SetWebhookSubscriptionActive(ctx, sub.ID, true); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	subs, err = repo.ListWebhookSubscriptionsByTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 || !subs[0].Active {
		t.Fatalf("after reactivate, Active = %v, want true", subs[0].Active)
	}

	err = repo.SetWebhookSubscriptionActive(ctx, uuid.NewString(), false)
	if !errors.Is(err, domain.ErrWebhookSubscriptionNotFound) {
		t.Errorf("set active on unknown id: err = %v, want ErrWebhookSubscriptionNotFound", err)
	}
}

// TestListWebhookSubscriptionsByTenantIsolatesTenants proves a tenant's list
// never includes another tenant's subscriptions.
func TestListWebhookSubscriptionsByTenantIsolatesTenants(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenantA, "webhook isolation test tenant A"); err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	if err := repo.CreateTenant(ctx, tenantB, "webhook isolation test tenant B"); err != nil {
		t.Fatalf("create tenant B: %v", err)
	}

	subA := domain.WebhookSubscription{TenantID: tenantA, URL: "https://a.example.com/hooks"}
	if err := repo.CreateWebhookSubscription(ctx, &subA, "whsec_a"); err != nil {
		t.Fatalf("create subscription A: %v", err)
	}
	subB := domain.WebhookSubscription{TenantID: tenantB, URL: "https://b.example.com/hooks"}
	if err := repo.CreateWebhookSubscription(ctx, &subB, "whsec_b"); err != nil {
		t.Fatalf("create subscription B: %v", err)
	}

	gotA, err := repo.ListWebhookSubscriptionsByTenant(ctx, tenantA)
	if err != nil {
		t.Fatalf("list tenant A: %v", err)
	}
	if len(gotA) != 1 || gotA[0].ID != subA.ID {
		t.Fatalf("tenant A subscriptions = %v, want exactly [%s]", gotA, subA.ID)
	}
}
