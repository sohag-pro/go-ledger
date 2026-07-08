package auth

import (
	"context"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

func TestTenantFromContext_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx := WithTenant(context.Background(), "tenant-1")

	got, ok := TenantFromContext(ctx)
	if !ok {
		t.Fatalf("TenantFromContext ok = false, want true")
	}
	if got != "tenant-1" {
		t.Fatalf("TenantFromContext = %q, want %q", got, "tenant-1")
	}
}

func TestTenantFromContext_Missing(t *testing.T) {
	t.Parallel()

	got, ok := TenantFromContext(context.Background())
	if ok {
		t.Fatalf("TenantFromContext ok = true, want false")
	}
	if got != "" {
		t.Fatalf("TenantFromContext = %q, want empty string", got)
	}
}

func TestKeyFromContext_RoundTrip(t *testing.T) {
	t.Parallel()

	want := domain.APIKey{ID: "key-1", TenantID: "tenant-1", Name: "test key"}
	ctx := WithKey(context.Background(), want)

	got, ok := KeyFromContext(ctx)
	if !ok {
		t.Fatalf("KeyFromContext ok = false, want true")
	}
	if got != want {
		t.Fatalf("KeyFromContext = %+v, want %+v", got, want)
	}
}

func TestKeyFromContext_Missing(t *testing.T) {
	t.Parallel()

	got, ok := KeyFromContext(context.Background())
	if ok {
		t.Fatalf("KeyFromContext ok = true, want false")
	}
	if got != (domain.APIKey{}) {
		t.Fatalf("KeyFromContext = %+v, want zero value", got)
	}
}
