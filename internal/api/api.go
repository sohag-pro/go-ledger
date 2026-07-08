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

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// APIVersion is the OpenAPI info.version. It is a deliberately stable semantic
// version, never a build SHA or timestamp, so the committed api/openapi.yaml
// snapshot stays reproducible across machines and CI.
const APIVersion = "0.2.0"

// MaxRequestBodyBytes bounds every request body, so one request can no longer
// exhaust memory before validation runs (see ADR-012, "Input hardening").
// cmd/server applies the same limit as a router-level middleware, ahead of
// huma, and the write operations below set it again as their own
// MaxBodyBytes so huma's own accounting reaches its limit (and returns a
// clean 413) before an oversized-but-declared body ever reaches the router
// middleware's harder cutoff.
const MaxRequestBodyBytes int64 = 64 * 1024

// Deps are the services the operations call, plus the auth resolver every /v1
// request goes through to derive its tenant. The tenant is never a request
// field: HumaMiddleware resolves it from the bearer key and handlers read it
// back with auth.TenantFromContext. Spec generation passes a zero Deps because
// the handler bodies (and the middleware, which only runs on a served request)
// are never invoked while serializing the schema.
type Deps struct {
	Accounts     *ledger.AccountService
	Transactions *ledger.TransactionService
	Audit        *ledger.AuditService
	Auth         *auth.Resolver
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

	api := humachi.New(router, config)

	// Require a bearer API key on every /v1 operation and derive its tenant
	// from the resolved key. Health, openapi.json/yaml, and schemas are huma
	// operations on this same API, so the middleware scopes itself by the
	// matched operation's path rather than by chi-level routing (see
	// docs/adr/012-api-authentication-and-hardening.md and
	// internal/auth/middleware.go).
	api.UseMiddleware(auth.HumaMiddleware(api, deps.Auth, slog.Default()))

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
}

// SpecYAML builds the API on a throwaway router and serializes its OpenAPI spec
// to YAML. It is the single source for both the generator (cmd/genopenapi) and
// the drift test, so the committed snapshot and the runtime spec come from the
// same code path.
func SpecYAML() ([]byte, error) {
	api := New(chi.NewRouter(), Deps{})
	return api.OpenAPI().YAML()
}
