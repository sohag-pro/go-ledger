package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/fx"
)

// --- rates ---

// CreateFXRateInput is the create-fx-rate request body.
type CreateFXRateInput struct {
	Body struct {
		TenantID  string `json:"tenant_id,omitempty" doc:"Tenant to scope this rate to. Omit or empty for the global default."`
		Base      string `json:"base" pattern:"^[A-Z]{3}$" doc:"Base currency, 3 uppercase letters"`
		Quote     string `json:"quote" pattern:"^[A-Z]{3}$" doc:"Quote currency, 3 uppercase letters"`
		MidRateE8 int64  `json:"mid_rate_e8" minimum:"1" doc:"Mid rate, quote units per base unit, scaled by 1e8"`
		SpreadBps *int32 `json:"spread_bps,omitempty" minimum:"0" maximum:"9999" doc:"Optional per-pair markup in basis points. Omit to use the applicable markup default."`
	}
}

// FXRateBody is the JSON shape of an FX rate in the create-fx-rate and
// list-fx-rates responses.
type FXRateBody struct {
	TenantID           string    `json:"tenant_id" doc:"Tenant id, empty for the global default"`
	Base               string    `json:"base"`
	Quote              string    `json:"quote"`
	MidRateE8          int64     `json:"mid_rate_e8"`
	SpreadBps          *int32    `json:"spread_bps" doc:"Per-pair override, null when the row uses the markup default"`
	EffectiveSpreadBps int32     `json:"effective_spread_bps" doc:"Spread a conversion would actually apply: override, else default, else 0"`
	Source             string    `json:"source"`
	EffectiveAt        time.Time `json:"effective_at"`
}

// FXRateOutput wraps a rate in a create-fx-rate response.
type FXRateOutput struct{ Body FXRateBody }

// ListFXRatesInput is the list-fx-rates request: an optional tenant filter.
type ListFXRatesInput struct {
	TenantID string `query:"tenant_id" doc:"Tenant whose overrides to include alongside the global defaults. Omit for globals only."`
}

// ListFXRatesOutput is the list-fx-rates response.
type ListFXRatesOutput struct {
	Body struct {
		Rates []FXRateBody `json:"rates"`
	}
}

func toFXRateBody(v fx.RateView) FXRateBody {
	return FXRateBody{
		TenantID: v.TenantID, Base: v.Base, Quote: v.Quote, MidRateE8: v.MidRateE8,
		SpreadBps: v.SpreadBps, EffectiveSpreadBps: v.EffectiveSpreadBps,
		Source: v.Source, EffectiveAt: v.EffectiveAt,
	}
}

// --- markup ---

// SetFXMarkupInput is the set-fx-markup request body.
type SetFXMarkupInput struct {
	Body struct {
		TenantID         string `json:"tenant_id,omitempty" doc:"Tenant to scope this default to. Omit or empty for the global default."`
		DefaultSpreadBps int32  `json:"default_spread_bps" minimum:"0" maximum:"9999" doc:"Default markup in basis points applied when a rate carries no spread"`
	}
}

// FXMarkupBody is the JSON shape of a markup default in the set-fx-markup
// and get-fx-markup responses.
type FXMarkupBody struct {
	DefaultSpreadBps int32     `json:"default_spread_bps"`
	EffectiveAt      time.Time `json:"effective_at"`
}

// SetFXMarkupOutput wraps a markup default in a set-fx-markup response.
type SetFXMarkupOutput struct{ Body FXMarkupBody }

// GetFXMarkupInput is the get-fx-markup request: an optional tenant filter.
type GetFXMarkupInput struct {
	TenantID string `query:"tenant_id" doc:"Tenant whose override to return alongside the global default. Omit for global only."`
}

// nullableFXMarkupBody wraps *FXMarkupBody so its OpenAPI schema is "the
// FXMarkupBody schema, or null" (anyOf), matching what GetMarkup actually
// returns: no global default set yet, or no tenant override, legitimately
// come back as JSON null rather than an object. huma's own "nullable:true"
// struct tag panics at Register time for a field typed as a $ref to an
// object schema ("nullable is not supported for..."), since it only knows
// how to fold null into a scalar/array type, not a $ref; SchemaProvider is
// huma's documented escape hatch for a case like this.
type nullableFXMarkupBody struct {
	*FXMarkupBody
}

// Schema implements huma.SchemaProvider.
func (nullableFXMarkupBody) Schema(r huma.Registry) *huma.Schema {
	ref := r.Schema(reflect.TypeOf(FXMarkupBody{}), true, "FXMarkupBody")
	return &huma.Schema{AnyOf: []*huma.Schema{ref, {Type: "null"}}}
}

// MarshalJSON renders the wrapped pointer exactly as *FXMarkupBody would:
// the object, or JSON null.
func (n nullableFXMarkupBody) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.FXMarkupBody)
}

// UnmarshalJSON is the mirror of MarshalJSON, for symmetry and for tests or
// clients that decode a get-fx-markup response back into this type.
func (n *nullableFXMarkupBody) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		n.FXMarkupBody = nil
		return nil
	}
	v := new(FXMarkupBody)
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	n.FXMarkupBody = v
	return nil
}

// GetFXMarkupOutput is the get-fx-markup response.
type GetFXMarkupOutput struct {
	Body struct {
		Global nullableFXMarkupBody `json:"global"`
		Tenant nullableFXMarkupBody `json:"tenant"`
	}
}

// registerFXAdmin wires the live FX config surface (ADR-020) under /v1/admin/fx.
// Every route is under /v1/admin/, so auth.HumaMiddleware already requires the
// admin scope; the operations add no further auth beyond bearerSecurity.
func registerFXAdmin(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "create-fx-rate",
		Method:        http.MethodPost,
		Path:          "/v1/admin/fx/rates",
		Summary:       "Set an FX rate",
		Description:   "Appends a rate for a directed pair (base to quote). tenant_id omitted or empty sets the global default; a tenant_id scopes it to that tenant, which the provider resolves ahead of the global default. spread_bps is an optional per-pair markup; omit it to fall back to the markup default. Append-only: this never updates an existing row.",
		Tags:          []string{"admin: fx"},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *CreateFXRateInput) (*FXRateOutput, error) {
		v, err := deps.FX.InsertRate(ctx, in.Body.TenantID, in.Body.Base, in.Body.Quote, in.Body.MidRateE8, in.Body.SpreadBps)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &FXRateOutput{Body: toFXRateBody(v)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-fx-rates",
		Method:      http.MethodGet,
		Path:        "/v1/admin/fx/rates",
		Summary:     "List current FX rates",
		Description: "Returns the current effective rate per pair for the given tenant (its overrides plus the global defaults), each with the spread a conversion would actually apply.",
		Tags:        []string{"admin: fx"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *ListFXRatesInput) (*ListFXRatesOutput, error) {
		rates, err := deps.FX.ListRates(ctx, in.TenantID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &ListFXRatesOutput{}
		out.Body.Rates = make([]FXRateBody, 0, len(rates))
		for _, r := range rates {
			out.Body.Rates = append(out.Body.Rates, toFXRateBody(r))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "set-fx-markup",
		Method:        http.MethodPost,
		Path:          "/v1/admin/fx/markup",
		Summary:       "Set the default FX markup",
		Description:   "Appends a default markup (basis points) applied to any conversion whose rate carries no per-pair spread. tenant_id omitted or empty sets the global default; a tenant_id scopes it to that tenant. Append-only.",
		Tags:          []string{"admin: fx"},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *SetFXMarkupInput) (*SetFXMarkupOutput, error) {
		d, err := deps.FX.SetMarkup(ctx, in.Body.TenantID, in.Body.DefaultSpreadBps)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &SetFXMarkupOutput{Body: FXMarkupBody{DefaultSpreadBps: d.DefaultSpreadBps, EffectiveAt: d.EffectiveAt}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-fx-markup",
		Method:      http.MethodGet,
		Path:        "/v1/admin/fx/markup",
		Summary:     "Get the default FX markup",
		Description: "Returns the current global default markup and, when tenant_id is given, that tenant's own override (null if it has none).",
		Tags:        []string{"admin: fx"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *GetFXMarkupInput) (*GetFXMarkupOutput, error) {
		v, err := deps.FX.GetMarkup(ctx, in.TenantID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &GetFXMarkupOutput{}
		if v.Global != nil {
			out.Body.Global = nullableFXMarkupBody{FXMarkupBody: &FXMarkupBody{DefaultSpreadBps: v.Global.DefaultSpreadBps, EffectiveAt: v.Global.EffectiveAt}}
		}
		if v.Tenant != nil {
			out.Body.Tenant = nullableFXMarkupBody{FXMarkupBody: &FXMarkupBody{DefaultSpreadBps: v.Tenant.DefaultSpreadBps, EffectiveAt: v.Tenant.EffectiveAt}}
		}
		return out, nil
	})
}
