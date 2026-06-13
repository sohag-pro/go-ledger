// Package api defines the go-ledger HTTP API using huma, which generates the
// OpenAPI 3.1 spec directly from the typed operation handlers. The spec is a
// byproduct of the same handlers that serve traffic, so the published docs and
// the API cannot drift. huma runs on the stdlib mux today via the humago
// adapter; when chi lands (Week 5) this swaps to the humachi adapter without
// touching the operations.
package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// APIVersion is the OpenAPI info.version. It is a deliberately stable semantic
// version, never a build SHA or timestamp, so the committed api/openapi.yaml
// snapshot stays reproducible across machines and CI.
const APIVersion = "0.1.0"

// New builds the huma API, registers every operation onto mux, and returns the
// API. huma also serves /openapi.json, /openapi.yaml, and /schemas/ from the
// same mux. The interactive playground is registered separately by
// RegisterPlayground.
func New(mux *http.ServeMux) huma.API {
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
		"Every endpoint here is generated from the live Go handlers, so this spec always matches the running service."
	config.Info.Contact = &huma.Contact{Name: "Sohag Hasan", URL: "https://sohag.pro"}
	config.Info.License = &huma.License{
		Name: "MIT",
		URL:  "https://github.com/sohag-pro/go-ledger/blob/main/LICENSE",
	}
	config.Servers = []*huma.Server{
		{URL: "https://go.sohag.pro", Description: "production"},
	}

	api := humago.New(mux, config)
	registerOperations(api)
	return api
}

// registerOperations wires every API operation. New endpoints get added here
// and show up in the spec and playground automatically.
func registerOperations(api huma.API) {
	registerHealth(api)
}

// SpecYAML builds the API on a throwaway mux and serializes its OpenAPI spec to
// YAML. It is the single source for both the generator (cmd/genopenapi) and the
// drift test, so the committed snapshot and the runtime spec come from the same
// code path.
func SpecYAML() ([]byte, error) {
	api := New(http.NewServeMux())
	return api.OpenAPI().YAML()
}
