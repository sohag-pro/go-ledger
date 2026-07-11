package web

import (
	"encoding/json"
	"net/http"
)

// ConsoleConfigData is the small, unauthenticated signal the operator console
// reads to decide its mode: whether admin is public (demo) or requires an
// entered admin key (production), and which tenant to default to.
type ConsoleConfigData struct {
	DemoMode        bool   `json:"demo_mode"`
	DefaultTenantID string `json:"default_tenant_id"`
}

// ConsoleConfig returns a handler that serves the given console config as JSON.
// It carries no secret and requires no auth.
func ConsoleConfig(data ConsoleConfigData) http.HandlerFunc {
	body, _ := json.Marshal(data)
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	}
}
