package auth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// authHeader is the header a bearer token is read from. Handlers and this
// middleware never log its value, only the outcome of resolving it.
const authHeader = "Authorization"

// v1PathPrefix is the only path prefix that requires a bearer API key. Health,
// the OpenAPI/schema documents, the playground, the console, and static assets
// are huma operations or chi routes outside this prefix and are deliberately
// left open (see docs/adr/012-api-authentication-and-hardening.md).
const v1PathPrefix = "/v1"

// isV1Path reports whether path is under /v1, treating "/v1" itself and any
// "/v1/..." path as in scope, but not an unrelated path that merely starts
// with the same four characters (e.g. "/v1beta").
func isV1Path(path string) bool {
	return path == v1PathPrefix || strings.HasPrefix(path, v1PathPrefix+"/")
}

// HumaMiddleware returns a huma middleware, registered with api.UseMiddleware,
// that requires a valid bearer API key on every operation whose path is under
// /v1 and lets every other operation (health, openapi.json/yaml, schemas)
// through unauthenticated. Health and openapi are huma operations on the same
// API as the rest of /v1, so this middleware scopes itself by the matched
// operation's path rather than relying on chi-level routing.
//
// On success it also enforces the resolved key's scope (Task 2.2), via
// RequiredHTTPScope and CheckScope: a key missing the scope its operation's
// method or path requires is rejected with 403, the same shape as the
// tenant-status gate below, since the credential itself is valid, it just
// lacks the scope. Only once both gates pass does it derive the tenant from
// the resolved key and inject both the tenant id and the key into the request
// context (WithTenant, WithKey) so downstream handlers read the tenant with
// TenantFromContext instead of trusting any request field: the tenant comes
// only from the key. On any failure it writes a problem+json body and never
// calls next, so the handler body never runs.
//
// Before any of that, if throttle is non-nil, it is checked against the
// caller's IP (via clientIPFromHuma) and, if that IP is over its failed-auth
// budget, the request is rejected with 401 WITHOUT calling resolver at all
// (Task 5.2, audit A2.5/A6.4): this is what stops a garbage-API-key flood
// from turning into one database lookup per bad key, since resolver never
// caches a miss (see Resolver's own doc comment on that trade). A resolve
// that fails with ErrUnauthorized records a failure against the caller's IP;
// a resolve that succeeds clears it. throttle may be nil (spec generation,
// and tests that only exercise unauthenticated routes), in which case this
// entire gate is skipped, matching how a nil RateLimiter is handled
// downstream.
//
// api is the same huma.API this middleware is registered on; huma.WriteErr
// needs it for content negotiation when writing the error body.
func HumaMiddleware(api huma.API, resolver *Resolver, throttle *NegativeThrottle, log *slog.Logger) func(huma.Context, func(huma.Context)) {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx huma.Context, next func(huma.Context)) {
		op := ctx.Operation()
		if op == nil || !isV1Path(op.Path) {
			next(ctx)
			return
		}

		if resolver == nil {
			log.LogAttrs(ctx.Context(), slog.LevelError, "auth: no resolver configured for a /v1 request",
				slog.String("path", op.Path))
			_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "")
			return
		}

		ip := clientIPFromHuma(ctx)
		if throttle != nil && !throttle.Allowed(ip) {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "")
			return
		}

		key, err := resolver.Resolve(ctx.Context(), ctx.Header(authHeader))
		if err != nil {
			if throttle != nil && errors.Is(err, ErrUnauthorized) {
				throttle.RecordFailure(ip)
			}
			var tenantErr *domain.TenantNotActiveError
			if errors.As(err, &tenantErr) {
				_ = huma.WriteErr(api, ctx, http.StatusForbidden, tenantErr.Reason())
				return
			}
			if errors.Is(err, ErrUnauthorized) {
				_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "")
				return
			}
			log.LogAttrs(ctx.Context(), slog.LevelError, "auth: resolve failed",
				slog.String("path", op.Path), slog.String("error", err.Error()))
			_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "")
			return
		}
		if throttle != nil {
			throttle.RecordSuccess(ip)
		}

		required := RequiredHTTPScope(op.Method, op.Path)
		if scopeErr := CheckScope(key, required); scopeErr != nil {
			var insufficientErr *domain.InsufficientScopeError
			errors.As(scopeErr, &insufficientErr)
			_ = huma.WriteErr(api, ctx, http.StatusForbidden, insufficientErr.Reason())
			return
		}

		newCtx := WithKey(WithTenant(ctx.Context(), key.TenantID), key)
		next(huma.WithContext(ctx, newCtx))
	}
}

// Middleware returns a net/http middleware equivalent to HumaMiddleware: it
// resolves the Authorization header on every request through it, injecting
// the tenant and key into the request context on success or writing a 401
// problem+json body on failure. It is the chi-level fallback described in
// docs/adr/012 for a huma version where operation middleware cannot scope by
// path or mutate the downstream context; go-ledger's huma v2.38.0 does both
// cleanly (huma.Context.Operation().Path and huma.WithContext), so this
// function is not wired into cmd/server today, but is kept ready if a huma
// upgrade or a non-huma route ever needs chi-level auth.
//
// throttle, if non-nil, gates the resolver the same way it does in
// HumaMiddleware: an IP (via clientIP) over its failed-auth budget is
// rejected with 401 before resolver is called at all, a resolve failing with
// ErrUnauthorized records a failure, and a successful resolve clears it. See
// HumaMiddleware's doc comment for the full reasoning (Task 5.2, audit
// A2.5/A6.4); throttle may be nil to skip this gate entirely.
func Middleware(resolver *Resolver, throttle *NegativeThrottle, log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if throttle != nil && !throttle.Allowed(ip) {
				writeUnauthorized(w)
				return
			}

			key, err := resolver.Resolve(r.Context(), r.Header.Get(authHeader))
			if err != nil {
				if throttle != nil && errors.Is(err, ErrUnauthorized) {
					throttle.RecordFailure(ip)
				}
				var tenantErr *domain.TenantNotActiveError
				if errors.As(err, &tenantErr) {
					writeForbidden(w, tenantErr.Reason())
					return
				}
				if !errors.Is(err, ErrUnauthorized) {
					log.LogAttrs(r.Context(), slog.LevelError, "auth: resolve failed",
						slog.String("path", r.URL.Path), slog.String("error", err.Error()))
				}
				writeUnauthorized(w)
				return
			}
			if throttle != nil {
				throttle.RecordSuccess(ip)
			}

			required := RequiredHTTPScope(r.Method, r.URL.Path)
			if scopeErr := CheckScope(key, required); scopeErr != nil {
				var insufficientErr *domain.InsufficientScopeError
				errors.As(scopeErr, &insufficientErr)
				writeForbidden(w, insufficientErr.Reason())
				return
			}

			ctx := WithKey(WithTenant(r.Context(), key.TenantID), key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeUnauthorized writes the same 401 problem+json shape as HumaMiddleware,
// for the net/http fallback path which has no huma.API to negotiate content
// through.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"status":401,"title":"Unauthorized"}`))
}

// writeForbidden writes a 403 problem+json body naming reason (e.g. "tenant
// is suspended"), for the net/http fallback path which has no huma.API to
// negotiate content through. Unlike writeUnauthorized this includes a detail
// field: the credential was valid, so naming why the tenant is gated does not
// help an attacker enumerate keys the way distinguishing 401 causes would.
func writeForbidden(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusForbidden)
	body, _ := json.Marshal(map[string]any{"status": http.StatusForbidden, "title": "Forbidden", "detail": reason})
	_, _ = w.Write(body)
}
