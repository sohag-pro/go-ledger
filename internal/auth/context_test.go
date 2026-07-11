package auth

import (
	"context"
	"reflect"
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
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("KeyFromContext = %+v, want %+v", got, want)
	}
}

func TestKeyFromContext_Missing(t *testing.T) {
	t.Parallel()

	got, ok := KeyFromContext(context.Background())
	if ok {
		t.Fatalf("KeyFromContext ok = true, want false")
	}
	if !reflect.DeepEqual(got, domain.APIKey{}) {
		t.Fatalf("KeyFromContext = %+v, want zero value", got)
	}
}

// TestSetRequestLogInfo_WritesThroughThePointer proves the write-back
// mechanism follow-up F2 relies on (audit A6.3 partial): WithRequestLogInfo
// installs a *RequestLogInfo on ctx, and SetRequestLogInfo, given any
// context derived from it (mirroring how HumaMiddleware only ever sees a
// context derived from the one the logging middleware built), mutates the
// SAME box the original caller is still holding.
func TestSetRequestLogInfo_WritesThroughThePointer(t *testing.T) {
	t.Parallel()

	info := &RequestLogInfo{}
	ctx := WithRequestLogInfo(context.Background(), info)
	// A further-derived context, the same shape ctx.Context() takes on by
	// the time it reaches HumaMiddleware (see WithTenant/WithKey/huma.
	// WithContext in HumaMiddleware).
	derived := WithTenant(ctx, "tenant-derived")

	SetRequestLogInfo(derived, "key-derived", "tenant-derived")

	if info.KeyID != "key-derived" {
		t.Errorf("KeyID = %q, want %q", info.KeyID, "key-derived")
	}
	if info.TenantID != "tenant-derived" {
		t.Errorf("TenantID = %q, want %q", info.TenantID, "tenant-derived")
	}
}

// TestSetRequestLogInfo_NoBoxIsNoop proves SetRequestLogInfo never panics or
// errors when ctx carries no RequestLogInfo box at all, the normal shape for
// any caller that never wired cmd/server's logging middleware (most tests,
// including every other one in this package).
func TestSetRequestLogInfo_NoBoxIsNoop(t *testing.T) {
	t.Parallel()

	SetRequestLogInfo(context.Background(), "key-1", "tenant-1") // must not panic
}
