package auth

import (
	"context"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// ctxKey namespaces this package's context values so they cannot collide with
// keys set by other packages.
type ctxKey int

const (
	tenantCtxKey ctxKey = iota
	keyCtxKey
)

// WithTenant returns a copy of ctx carrying the resolved tenant id. The
// authentication middleware sets this after a successful Resolve; nothing
// downstream may set or override it from request input.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey, tenantID)
}

// TenantFromContext returns the tenant id set by WithTenant, if any.
func TenantFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(tenantCtxKey).(string)
	return v, ok
}

// WithKey returns a copy of ctx carrying the resolved API key, so downstream
// components (notably the per-key rate limiter) can read its rate limit
// without a second lookup.
func WithKey(ctx context.Context, k domain.APIKey) context.Context {
	return context.WithValue(ctx, keyCtxKey, k)
}

// KeyFromContext returns the API key set by WithKey, if any.
func KeyFromContext(ctx context.Context) (domain.APIKey, bool) {
	v, ok := ctx.Value(keyCtxKey).(domain.APIKey)
	return v, ok
}
