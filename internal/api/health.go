package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// HealthOutput is the response for the liveness check. huma derives the
// response schema from the Body field's type. Revision is additive (Task
// 5.6a): status stays exactly "ok" so the deploy health check's grep for it
// keeps working unchanged; revision only adds a field callers can read to
// see which build is actually serving.
type HealthOutput struct {
	Body struct {
		Status   string `json:"status" example:"ok" doc:"Liveness status, \"ok\" while the service is serving"`
		Revision string `json:"revision" example:"a1b2c3d" doc:"Build revision (git short SHA) the running binary was built from; \"dev\" outside a real build"`
	}
}

// registerHealth wires GET /healthz. The response body's status field is
// always exactly "ok", matching the contract the deploy health check relies
// on; revision is threaded in from cmd/server (Deps.Revision) so operators
// can tell which build answered a given request.
func registerHealth(api huma.API, revision string) {
	huma.Register(api, huma.Operation{
		OperationID: "health-check",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Liveness check",
		Description: "Returns 200 with {\"status\":\"ok\",\"revision\":\"<build revision>\"} while the service is up. Used by the deploy pipeline and uptime monitoring.",
		Tags:        []string{"meta"},
	}, func(_ context.Context, _ *struct{}) (*HealthOutput, error) {
		out := &HealthOutput{}
		out.Body.Status = "ok"
		out.Body.Revision = revision
		return out, nil
	})
}
