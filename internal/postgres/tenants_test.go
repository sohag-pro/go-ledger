package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestCreateAndGetTenant proves the happy path: a created tenant round-trips
// with its name and defaults to active status.
func TestCreateAndGetTenant(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	id := uuid.NewString()
	if err := repo.CreateTenant(ctx, id, "Acme Corp"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	got, err := repo.GetTenant(ctx, id)
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID = %q, want %q", got.ID, id)
	}
	if got.Name != "Acme Corp" {
		t.Errorf("Name = %q, want %q", got.Name, "Acme Corp")
	}
	if got.Status != domain.TenantActive {
		t.Errorf("Status = %q, want %q (new tenants default to active)", got.Status, domain.TenantActive)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want a real timestamp")
	}
}

// TestCreateTenantDuplicateID proves creating a tenant with an id already in
// use surfaces domain.ErrTenantAlreadyExists, not a generic wrapped error.
func TestCreateTenantDuplicateID(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	id := uuid.NewString()
	if err := repo.CreateTenant(ctx, id, "First"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := repo.CreateTenant(ctx, id, "Second")
	if !errors.Is(err, domain.ErrTenantAlreadyExists) {
		t.Fatalf("second create with same id: got %v, want ErrTenantAlreadyExists", err)
	}
}

// TestGetTenantNotFound proves a lookup for an id with no row returns the
// typed domain.ErrTenantNotFound.
func TestGetTenantNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	_, err := repo.GetTenant(context.Background(), uuid.NewString())
	if !errors.Is(err, domain.ErrTenantNotFound) {
		t.Errorf("got %v, want ErrTenantNotFound", err)
	}
}

// TestSetTenantStatus proves an operator can suspend, close, and reactivate a
// tenant, and that each transition is visible on the next GetTenant.
func TestSetTenantStatus(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	id := uuid.NewString()
	if err := repo.CreateTenant(ctx, id, "Suspend Me"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	for _, status := range []domain.TenantStatus{domain.TenantSuspended, domain.TenantClosed, domain.TenantActive} {
		if err := repo.SetTenantStatus(ctx, id, status); err != nil {
			t.Fatalf("SetTenantStatus(%s): %v", status, err)
		}
		got, err := repo.GetTenant(ctx, id)
		if err != nil {
			t.Fatalf("get tenant after SetTenantStatus(%s): %v", status, err)
		}
		if got.Status != status {
			t.Errorf("status after SetTenantStatus(%s) = %q, want %q", status, got.Status, status)
		}
	}
}

// TestSetTenantStatusInvalidStatus proves an out-of-range status is rejected
// before it ever reaches the database (and thus before it could violate the
// tenants.status CHECK constraint).
func TestSetTenantStatusInvalidStatus(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	id := uuid.NewString()
	if err := repo.CreateTenant(ctx, id, "Bad Status"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	err := repo.SetTenantStatus(ctx, id, "pending")
	if !errors.Is(err, domain.ErrInvalidTenant) {
		t.Errorf("SetTenantStatus with invalid status: got %v, want ErrInvalidTenant", err)
	}
}

// TestSetTenantStatusNotFound proves setting the status of an id with no
// tenant row returns domain.ErrTenantNotFound rather than silently succeeding
// (an UPDATE matching zero rows is not an error at the SQL level).
func TestSetTenantStatusNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	err := repo.SetTenantStatus(context.Background(), uuid.NewString(), domain.TenantSuspended)
	if !errors.Is(err, domain.ErrTenantNotFound) {
		t.Errorf("got %v, want ErrTenantNotFound", err)
	}
}

// TestListTenants proves ListTenants returns created tenants up to limit.
func TestListTenants(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	ids := make([]string, 3)
	for i := range ids {
		ids[i] = uuid.NewString()
		if err := repo.CreateTenant(ctx, ids[i], "tenant"); err != nil {
			t.Fatalf("create tenant %d: %v", i, err)
		}
	}

	got, err := repo.ListTenants(ctx, 1000)
	if err != nil {
		t.Fatalf("list tenants: %v", err)
	}
	seen := make(map[string]bool, len(got))
	for _, tn := range got {
		seen[tn.ID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("ListTenants did not include created tenant %s", id)
		}
	}

	limited, err := repo.ListTenants(ctx, 1)
	if err != nil {
		t.Fatalf("list tenants limited: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("ListTenants(1) returned %d rows, want 1", len(limited))
	}
}
