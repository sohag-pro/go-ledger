package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// HealthOutput is the response for the liveness check. huma derives the
// response schema from the Body field's type.
type HealthOutput struct {
	Body struct {
		Status string `json:"status" example:"ok" doc:"Liveness status, \"ok\" while the service is serving"`
	}
}

// registerHealth wires GET /healthz. The response body is exactly
// {"status":"ok"}, matching the contract the deploy health check relies on.
func registerHealth(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "health-check",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Liveness check",
		Description: "Returns 200 with {\"status\":\"ok\"} while the service is up. Used by the deploy pipeline and uptime monitoring.",
		Tags:        []string{"meta"},
	}, func(_ context.Context, _ *struct{}) (*HealthOutput, error) {
		out := &HealthOutput{}
		out.Body.Status = "ok"
		return out, nil
	})
}
