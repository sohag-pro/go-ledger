package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// TenantBody is the JSON shape of a tenant in admin responses.
type TenantBody struct {
	ID     string `json:"id" doc:"Tenant id (UUID)"`
	Name   string `json:"name"`
	Status string `json:"status" doc:"One of: active, suspended, closed"`
}

func toTenantBody(t domain.Tenant) TenantBody {
	return TenantBody{ID: t.ID, Name: t.Name, Status: string(t.Status)}
}

// CreateTenantInput is the create-tenant request body.
type CreateTenantInput struct {
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"200" doc:"Human-readable tenant name"`
	}
}

// TenantOutput wraps a tenant in a response.
type TenantOutput struct {
	Body TenantBody
}

// ListTenantsOutput is the list-tenants response.
type ListTenantsOutput struct {
	Body struct {
		Tenants []TenantBody `json:"tenants"`
	}
}

// SetTenantStatusInput is the set-tenant-status request: a path id plus the
// new status in the body.
type SetTenantStatusInput struct {
	ID   string `path:"id" format:"uuid" doc:"Tenant id"`
	Body struct {
		Status string `json:"status" enum:"active,suspended,closed" doc:"New tenant status"`
	}
}

// EmptyOutput is the huma output for admin operations that report success
// with no response body, just a 204 No Content status.
type EmptyOutput struct{}

// KeyBody is the JSON shape of an api key in the list-keys response. It
// never carries the plaintext: that is never stored anywhere to return, only
// shown once, at issue or rotate time, in IssuedKeyBody below.
type KeyBody struct {
	ID         string     `json:"id" doc:"Key id (UUID)"`
	TenantID   string     `json:"tenant_id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at" doc:"Null if the key never expires"`
	LastUsedAt *time.Time `json:"last_used_at" doc:"Null if the key has never been used"`
	RevokedAt  *time.Time `json:"revoked_at" doc:"Null if the key is still live"`
	CreatedAt  time.Time  `json:"created_at"`
}

func toKeyBody(k domain.APIKey) KeyBody {
	return KeyBody{
		ID:         k.ID,
		TenantID:   k.TenantID,
		Name:       k.Name,
		Scopes:     scopesToStrs(k.Scopes),
		ExpiresAt:  k.ExpiresAt,
		LastUsedAt: k.LastUsedAt,
		RevokedAt:  k.RevokedAt,
		CreatedAt:  k.CreatedAt,
	}
}

// IssuedKeyBody is the JSON shape of a just-issued or just-rotated key: its
// metadata plus the plaintext credential itself, shown exactly once. Store
// it now: only its hash is ever persisted, so it cannot be recovered again.
type IssuedKeyBody struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at" doc:"Null if the key never expires"`
	Plaintext string     `json:"plaintext" doc:"The api key credential, e.g. for the Authorization: Bearer header. Shown once: store it now, it cannot be retrieved again."`
}

func toIssuedKeyBody(plaintext string, k domain.APIKey) IssuedKeyBody {
	return IssuedKeyBody{
		ID:        k.ID,
		TenantID:  k.TenantID,
		Name:      k.Name,
		Scopes:    scopesToStrs(k.Scopes),
		ExpiresAt: k.ExpiresAt,
		Plaintext: plaintext,
	}
}

// scopesToStrs converts []domain.Scope to []string for a JSON response body.
func scopesToStrs(scopes []domain.Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// scopesFromStrs converts a request body's []string to []domain.Scope.
// Values are not validated here: admin.Service.IssueKey rejects an empty or
// invalid scope list itself (admin.ErrInvalidScopes), mapped to 422 by
// toHumaErr, the same way every other domain validation error is.
func scopesFromStrs(raw []string) []domain.Scope {
	out := make([]domain.Scope, len(raw))
	for i, s := range raw {
		out[i] = domain.Scope(s)
	}
	return out
}

// IssueKeyInput is the issue-key request body.
type IssueKeyInput struct {
	Body struct {
		TenantID  string     `json:"tenant_id" format:"uuid" doc:"Tenant this key acts as"`
		Name      string     `json:"name" minLength:"1" maxLength:"200" doc:"Human-readable label for this key"`
		Scopes    []string   `json:"scopes" minItems:"1" doc:"One or more of: read, post, admin"`
		ExpiresAt *time.Time `json:"expires_at,omitempty" doc:"Optional expiry. Omit for a key that never expires."`
	}
}

// IssueKeyOutput wraps a newly issued or rotated key.
type IssueKeyOutput struct {
	Body IssuedKeyBody
}

type keyIDInput struct {
	ID string `path:"id" format:"uuid" doc:"Key id"`
}

// ListKeysInput is the list-keys request: a required tenant filter.
type ListKeysInput struct {
	TenantID string `query:"tenant_id" format:"uuid" required:"true" doc:"Tenant to list keys for"`
}

// ListKeysOutput is the list-keys response.
type ListKeysOutput struct {
	Body struct {
		Keys []KeyBody `json:"keys"`
	}
}

// registerAdmin wires the operator surface (Task 2.2b, audit A3.2/A2.3):
// onboarding a tenant and issuing/rotating/revoking its api keys, entirely
// over REST, with no raw SQL. Every route here is under /v1/admin/, so
// auth.HumaMiddleware (see internal/auth/scope.go, RequiredHTTPScope)
// already requires domain.ScopeAdmin for all of them: no additional
// per-operation auth is needed beyond the shared bearerSecurity requirement
// every /v1 operation sets.
func registerAdmin(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "create-tenant",
		Method:        http.MethodPost,
		Path:          "/v1/admin/tenants",
		Summary:       "Create a tenant",
		Tags:          []string{"admin"},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *CreateTenantInput) (*TenantOutput, error) {
		t, err := deps.Admin.CreateTenant(ctx, in.Body.Name)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &TenantOutput{Body: toTenantBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-tenants",
		Method:      http.MethodGet,
		Path:        "/v1/admin/tenants",
		Summary:     "List tenants",
		Tags:        []string{"admin"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, _ *struct{}) (*ListTenantsOutput, error) {
		tenants, err := deps.Admin.ListTenants(ctx)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &ListTenantsOutput{}
		out.Body.Tenants = make([]TenantBody, 0, len(tenants))
		for _, t := range tenants {
			out.Body.Tenants = append(out.Body.Tenants, toTenantBody(t))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "set-tenant-status",
		Method:        http.MethodPost,
		Path:          "/v1/admin/tenants/{id}/status",
		Summary:       "Suspend, close, or reactivate a tenant",
		Tags:          []string{"admin"},
		DefaultStatus: http.StatusNoContent,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *SetTenantStatusInput) (*EmptyOutput, error) {
		if err := deps.Admin.SetTenantStatus(ctx, in.ID, domain.TenantStatus(in.Body.Status)); err != nil {
			return nil, toHumaErr(err)
		}
		return &EmptyOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "issue-key",
		Method:        http.MethodPost,
		Path:          "/v1/admin/keys",
		Summary:       "Issue a new api key",
		Description:   "Mints a new api key for a tenant. The plaintext is returned exactly once, in this response, and is never stored or recoverable again: store it now.",
		Tags:          []string{"admin"},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *IssueKeyInput) (*IssueKeyOutput, error) {
		plaintext, key, err := deps.Admin.IssueKey(ctx, in.Body.TenantID, in.Body.Name, scopesFromStrs(in.Body.Scopes), in.Body.ExpiresAt)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &IssueKeyOutput{Body: toIssuedKeyBody(plaintext, key)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "rotate-key",
		Method:        http.MethodPost,
		Path:          "/v1/admin/keys/{id}/rotate",
		Summary:       "Rotate an api key",
		Description:   "Mints a replacement key with the same tenant, name, and scopes as an existing one. The old key is left active (an overlap window for callers to cut over); revoke it explicitly once they have.",
		Tags:          []string{"admin"},
		DefaultStatus: http.StatusCreated,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *keyIDInput) (*IssueKeyOutput, error) {
		plaintext, key, err := deps.Admin.RotateKey(ctx, in.ID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &IssueKeyOutput{Body: toIssuedKeyBody(plaintext, key)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "revoke-key",
		Method:        http.MethodPost,
		Path:          "/v1/admin/keys/{id}/revoke",
		Summary:       "Revoke an api key",
		Tags:          []string{"admin"},
		DefaultStatus: http.StatusNoContent,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *keyIDInput) (*EmptyOutput, error) {
		if err := deps.Admin.RevokeKey(ctx, in.ID); err != nil {
			return nil, toHumaErr(err)
		}
		return &EmptyOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-keys",
		Method:      http.MethodGet,
		Path:        "/v1/admin/keys",
		Summary:     "List a tenant's api keys",
		Description: "Lists every key for a tenant, live and revoked, oldest first. Never includes a plaintext: that is shown once, at issue or rotate time, and is never recoverable afterward.",
		Tags:        []string{"admin"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *ListKeysInput) (*ListKeysOutput, error) {
		keys, err := deps.Admin.ListKeys(ctx, in.TenantID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &ListKeysOutput{}
		out.Body.Keys = make([]KeyBody, 0, len(keys))
		for _, k := range keys {
			out.Body.Keys = append(out.Body.Keys, toKeyBody(k))
		}
		return out, nil
	})
}
