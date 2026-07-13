package web

import (
	"encoding/json"
	"net/http"
)

// ConsoleConfigData is the small, unauthenticated signal the operator console
// reads to decide its mode: whether admin is public (demo) or requires an
// entered admin key (production), which tenant to default to, and the
// server's approval policy (ADR-025), surfaced read-only so the console's
// Policy panel can explain what governs the Approvals panel without a
// dedicated authenticated endpoint. None of this is a secret: thresholds and
// whether the gate is on are operational facts about the deployment, not
// per-tenant data.
type ConsoleConfigData struct {
	DemoMode        bool   `json:"demo_mode"`
	DefaultTenantID string `json:"default_tenant_id"`

	// ApprovalEnabled mirrors APPROVAL_ENABLED: false means the gate never
	// fires and every post/convert/reverse behaves exactly as it did before
	// approval workflows existed.
	ApprovalEnabled bool `json:"approval_enabled"`
	// ApprovalThresholds mirrors APPROVAL_THRESHOLDS, currency to minor
	// units. A currency absent here is never gated.
	ApprovalThresholds map[string]int64 `json:"approval_thresholds,omitempty"`
	// ApprovalRequireDifferentActor mirrors APPROVAL_REQUIRE_DIFFERENT_ACTOR:
	// whether an approval decision must come from someone other than the
	// original requester.
	ApprovalRequireDifferentActor bool `json:"approval_require_different_actor"`
	// ApprovalTTLSeconds mirrors PENDING_TTL: how long an undecided pending
	// lives before the background sweep expires it.
	ApprovalTTLSeconds int64 `json:"approval_ttl_seconds"`
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
