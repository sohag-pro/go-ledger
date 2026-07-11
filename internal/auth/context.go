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
	requestLogInfoCtxKey
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

// RequestLogInfo is a mutable box the access-log middleware (cmd/server's
// slogLogger) installs on a request's context BEFORE the request reaches
// HumaMiddleware, so the key id and tenant id it resolves can flow back out
// to the logger after the request completes (follow-up F2, audit A6.3
// partial). This exists because HumaMiddleware runs deep inside huma's own
// request pipeline, on a huma.Context whose Context() is a fresh child
// context (see huma.WithContext in HumaMiddleware): plumbing a return value
// back up through huma's handler dispatch is not an option, but a pointer
// stored in the ORIGINAL context is shared by every context derived from it,
// including that child, so writing through the pointer here is visible to
// whoever holds the original one. Deliberately carries only the key id and
// tenant id (both non-secret identifiers), never the key's plaintext or
// hash: see SetRequestLogInfo's doc comment for why nothing else belongs
// here.
type RequestLogInfo struct {
	KeyID    string
	TenantID string
}

// WithRequestLogInfo returns a copy of ctx carrying info. Call this once, in
// the outermost logging middleware, before the request reaches
// HumaMiddleware; SetRequestLogInfo then fills info's fields in place once
// authentication resolves, and the same *info the logging middleware
// retained lets it read those fields back after the request completes.
func WithRequestLogInfo(ctx context.Context, info *RequestLogInfo) context.Context {
	return context.WithValue(ctx, requestLogInfoCtxKey, info)
}

// SetRequestLogInfo writes keyID and tenantID into the RequestLogInfo box
// installed on ctx by WithRequestLogInfo, if one is present. It is a no-op
// (never an error, never a panic) when none is present, which is the normal
// case for any caller that did not go through cmd/server's logging
// middleware, for example a test that exercises HumaMiddleware directly.
// This is the ONLY thing HumaMiddleware ever writes into the box: never the
// key's plaintext or hash, only its id, since the box's contents end up in a
// structured log line and a credential must never appear there.
func SetRequestLogInfo(ctx context.Context, keyID, tenantID string) {
	if info, ok := ctx.Value(requestLogInfoCtxKey).(*RequestLogInfo); ok {
		info.KeyID = keyID
		info.TenantID = tenantID
	}
}
