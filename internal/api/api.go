// Package api defines the go-ledger HTTP API using huma, which generates the
// OpenAPI 3.1 spec directly from the typed operation handlers. The spec is a
// byproduct of the same handlers that serve traffic, so the published docs and
// the API cannot drift. huma runs on a chi router via the humachi adapter, so
// chi owns routing and middleware while huma owns operations, validation, and
// problem+json errors.
package api

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// APIVersion is the OpenAPI info.version. It is a deliberately stable semantic
// version, never a build SHA or timestamp, so the committed api/openapi.yaml
// snapshot stays reproducible across machines and CI.
const APIVersion = "0.2.0"

// Deps are the services the operations call, plus the tenant every request acts
// as until an auth layer resolves a real one. Spec generation passes a zero Deps
// because the handler bodies are never invoked while serializing the schema.
type Deps struct {
	Accounts      *ledger.AccountService
	Transactions  *ledger.TransactionService
	DefaultTenant string
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
	registerOperations(api, deps)
	return api
}

// registerOperations wires every API operation. New endpoints get added here and
// show up in the spec and playground automatically.
func registerOperations(api huma.API, deps Deps) {
	registerHealth(api)
	registerAccounts(api, deps)
	registerTransactions(api, deps)
}

// SpecYAML builds the API on a throwaway router and serializes its OpenAPI spec
// to YAML. It is the single source for both the generator (cmd/genopenapi) and
// the drift test, so the committed snapshot and the runtime spec come from the
// same code path.
func SpecYAML() ([]byte, error) {
	api := New(chi.NewRouter(), Deps{})
	return api.OpenAPI().YAML()
}
