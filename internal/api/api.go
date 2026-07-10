// Package api defines the go-ledger HTTP API using huma, which generates the
// OpenAPI 3.1 spec directly from the typed operation handlers. The spec is a
// byproduct of the same handlers that serve traffic, so the published docs and
// the API cannot drift. huma runs on a chi router via the humachi adapter, so
// chi owns routing and middleware while huma owns operations, validation, and
// problem+json errors.
package api

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// APIVersion is the OpenAPI info.version. It is a deliberately stable semantic
// version, never a build SHA or timestamp, so the committed api/openapi.yaml
// snapshot stays reproducible across machines and CI.
const APIVersion = "0.2.0"

// MaxRequestBodyBytes bounds every request body, so one request can no longer
// exhaust memory before validation runs (see ADR-012, "Input hardening").
// cmd/server applies the same limit as a router-level maxBodyBytes
// middleware that wraps huma, so it runs first and enforces the hard cutoff.
// The write operations below also set MaxBodyBytes as their own huma option,
// so huma's own accounting returns a clean 413 for the same limit when it is
// the one reading the body, such as in tests that exercise the huma router
// directly without the wrapping middleware.
const MaxRequestBodyBytes int64 = 64 * 1024

// bearerSecurity is the per-operation Security requirement every /v1
// operation sets, referencing the "bearerAuth" scheme registered on
// config.Components.SecuritySchemes in New. huma's OpenAPI security model
// is per-operation (an empty inner slice means no scopes, which is right
// for a plain bearer token), so this is attached individually to each /v1
// huma.Operation literal rather than as a single global default; health,
// openapi.json/yaml, and schemas simply never set it.
var bearerSecurity = []map[string][]string{{"bearerAuth": {}}}

// Deps are the services the operations call, plus the auth resolver every /v1
// request goes through to derive its tenant, and the per-key rate limiter
// applied after auth. The tenant is never a request field: HumaMiddleware
// resolves it from the bearer key and handlers read it back with
// auth.TenantFromContext. Spec generation passes a zero Deps because the
// handler bodies (and the middleware, which only runs on a served request)
// are never invoked while serializing the schema.
type Deps struct {
	Accounts     *ledger.AccountService
	Transactions *ledger.TransactionService
	Audit        *ledger.AuditService
	// Admin backs the /v1/admin operations (Task 2.2b): onboarding a tenant
	// and issuing/rotating/revoking its api keys. Every /v1/admin/ path
	// already requires domain.ScopeAdmin via auth.HumaMiddleware
	// (RequiredHTTPScope), so the operations themselves add no further auth.
	Admin *admin.Service
	Auth  *auth.Resolver
	// RateLimiter, if set, is registered immediately after the auth middleware
	// (see New). It is optional: a zero Deps (spec generation, and tests that
	// only exercise unauthenticated routes) leaves it nil and no rate-limit
	// middleware is registered at all, rather than one that always fails open.
	RateLimiter *auth.Limiter
	// NegativeThrottle, if set, is passed straight through to
	// auth.HumaMiddleware (Task 5.2, audit A2.5/A6.4): it caps failed-auth
	// attempts per client IP BEFORE the resolver's database lookup runs, so a
	// garbage-API-key flood cannot exhaust the connection pool. Optional for
	// the same reason RateLimiter is: a nil value skips the gate entirely
	// rather than failing closed.
	NegativeThrottle *auth.NegativeThrottle
}

// tenantFromCtx reads the tenant HumaMiddleware resolved from the caller's API
// key. Every /v1 handler calls this instead of trusting a request field: the
// tenant comes only from the key. A missing tenant means the middleware did
// not run, which should be impossible for a served /v1 request, so it is
// reported as an internal error rather than silently defaulting to any
// tenant.
func tenantFromCtx(ctx context.Context) (string, error) {
	tenant, ok := auth.TenantFromContext(ctx)
	if !ok {
		return "", huma.Error500InternalServerError("tenant not resolved")
	}
	return tenant, nil
}

// New builds the huma API on the given chi router, registers every operation,
// and returns the API. huma also serves /openapi.json, /openapi.yaml, and
// /schemas/ from the same router. The interactive playground is registered
// separately by RegisterPlayground.
func New(router chi.Router, deps Deps) huma.API {
	config := huma.DefaultConfig("go-ledger API", APIVersion)

	// Drop the default create hooks. The only one DefaultConfig adds is the
	// schema-link transformer, which injects a `$schema` field and Link header
	// into every response. We want a clean, stable response contract (for
	// example /healthz returning exactly {"status":"ok"}), so we remove it.
	config.CreateHooks = nil

	// Disable huma's built-in docs route (Stoplight Elements, loaded from a
	// CDN). We serve a self-hosted Scalar playground at /playground instead.
	config.DocsPath = ""

	config.Info.Description = "A production-grade payment ledger built on double-entry accounting. " +
		"Every endpoint here is generated from the live Go handlers, so this spec always matches the running service.\n\n" +
		"Amounts are signed integer minor units (e.g. cents) plus an ISO 4217 currency code. " +
		"A transaction's postings must sum to zero.\n\n" +
		"Authentication: every /v1 endpoint requires a bearer API key, sent as `Authorization: Bearer <api-key>`. " +
		"A missing, unknown, or revoked key returns 401. To try the API without provisioning your own key, use the public demo key " +
		"`glk_demo_public_key_reset_every_4h` (also wired into the try-it console and the Scalar playground below): it is scoped to " +
		"the demo tenant and rate limited, so it is safe to expose.\n\n" +
		"Idempotency: POST /v1/transactions requires an `Idempotency-Key` header so retries are safe. Repeating a request with the " +
		"same key returns the original transaction unchanged (with `Idempotent-Replayed: true`) instead of posting again; reusing a " +
		"key with a different request body returns 409 Conflict.\n\n" +
		"This is a public demo: the data resets every 4 hours to a fresh, realistic ledger.\n\n" +
		"- Source: [github.com/sohag-pro/go-ledger](https://github.com/sohag-pro/go-ledger)\n" +
		"- Landing page: [go.sohag.pro](https://go.sohag.pro)\n" +
		"- Try-it console: [go.sohag.pro/console](https://go.sohag.pro/console)"
	config.Info.Contact = &huma.Contact{Name: "Sohag Hasan", URL: "https://sohag.pro"}
	config.Info.License = &huma.License{
		Name: "MIT",
		URL:  "https://github.com/sohag-pro/go-ledger/blob/main/LICENSE",
	}
	config.Servers = []*huma.Server{
		{URL: "https://go.sohag.pro", Description: "production"},
	}

	// Advertise bearer-key auth in the spec itself, so the Scalar playground
	// renders an Authorization input and generated clients know the scheme.
	// This is declared once here and referenced by name (bearerSecurity,
	// below) from every /v1 operation's Security field. It is deliberately
	// not set as a global default: health, openapi.json/yaml, and schemas
	// are unauthenticated (see New's middleware comment), and per-operation
	// Security keeps that true in the spec instead of listing them as
	// exceptions to a blanket requirement.
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearerAuth": {
			Type:        "http",
			Scheme:      "bearer",
			Description: "API key issued per tenant. Send as `Authorization: Bearer <api-key>`. Use `glk_demo_public_key_reset_every_4h` to try the API against the demo tenant.",
		},
	}

	api := humachi.New(router, config)

	// Require a bearer API key on every /v1 operation and derive its tenant
	// from the resolved key. Health, openapi.json/yaml, and schemas are huma
	// operations on this same API, so the middleware scopes itself by the
	// matched operation's path rather than by chi-level routing (see
	// docs/adr/012-api-authentication-and-hardening.md and
	// internal/auth/middleware.go).
	api.UseMiddleware(auth.HumaMiddleware(api, deps.Auth, deps.NegativeThrottle, slog.Default()))

	// Rate limiting is registered immediately after auth, never before: it
	// reads the key auth.HumaMiddleware just resolved into the context
	// (auth.KeyFromContext) and fails OPEN when no key is present there (see
	// auth.Limiter.HumaMiddleware). If this ran before auth, a request with
	// no key at all would sail through the limiter and rely on auth alone to
	// reject it, which happens to still work today, but a key that IS
	// present would never be checked against its bucket, silently disabling
	// rate limiting. Registering it here, after auth, is what makes "valid
	// key over its limit" actually 429 (see docs/adr/012-api-authentication-and-hardening.md).
	if deps.RateLimiter != nil {
		api.UseMiddleware(deps.RateLimiter.HumaMiddleware(api))
	}

	registerOperations(api, deps)
	return api
}

// registerOperations wires every API operation. New endpoints get added here and
// show up in the spec and playground automatically.
func registerOperations(api huma.API, deps Deps) {
	registerHealth(api)
	registerAccounts(api, deps)
	registerTransactions(api, deps)
	registerAudit(api, deps)
	registerAdmin(api, deps)
}

// SpecYAML builds the API on a throwaway router and serializes its OpenAPI spec
// to YAML. It is the single source for both the generator (cmd/genopenapi) and
// the drift test, so the committed snapshot and the runtime spec come from the
// same code path.
func SpecYAML() ([]byte, error) {
	api := New(chi.NewRouter(), Deps{})
	return api.OpenAPI().YAML()
}
