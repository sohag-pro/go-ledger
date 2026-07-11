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

// SetTenantPolicyInput is the set-tenant-policy request (Task 2.4b, audit
// A3.4): a path id plus the full policy in the body. Every field is
// optional; an omitted field is 0/empty, meaning "no limit" for that
// guardrail (see domain.TenantPolicy). This call always writes the FULL
// policy the body describes, not a partial merge: omitting a field clears
// (unlimits) that guardrail rather than leaving a previous value in place.
type SetTenantPolicyInput struct {
	ID   string `path:"id" format:"uuid" doc:"Tenant id"`
	Body struct {
		MaxTransactionAmount int64    `json:"max_transaction_amount,omitempty" doc:"Max per-currency debit total for a single transaction, in minor units. 0 (or omitted) means unlimited."`
		DailyVolumeLimit     int64    `json:"daily_volume_limit,omitempty" doc:"Max per-currency cumulative debit total for the current day, in minor units. 0 (or omitted) means unlimited."`
		AllowedCurrencies    []string `json:"allowed_currencies,omitempty" doc:"Currencies this tenant may post in. Empty (or omitted) means every currency is allowed."`
	}
}

// EmptyOutput is the huma output for admin operations that report success
// with no response body, just a 204 No Content status.
type EmptyOutput struct{}

// ShredTenantPIIInput is the shred-pii request: a path id plus a required
// confirmation flag (Task 6.2, audit A9.3). Confirm must be exactly true:
// omitting it (the zero value, false) fails closed with a 400 rather than
// silently proceeding, since this call is irreversible (see
// admin.Service.ShredTenantPII's own doc comment). There is deliberately no
// way to pass this as a query parameter or header instead: a body field
// forces a caller to construct the request deliberately, not accidentally
// via a copy-pasted URL.
type ShredTenantPIIInput struct {
	ID   string `path:"id" format:"uuid" doc:"Tenant id"`
	Body struct {
		Confirm bool `json:"confirm" doc:"Must be true. This operation is irreversible: it permanently destroys the tenant's PII encryption key, and every posting description ever encrypted under it becomes permanently unreadable."`
	}
}

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

// WebhookSubscriptionBody is the JSON shape of a webhook subscription in the
// list-webhook-subscriptions response (Task 4.1, audit A7.1). It never
// carries the signing secret: that is shown once, at creation time, in
// IssuedWebhookSubscriptionBody below.
type WebhookSubscriptionBody struct {
	ID         string    `json:"id" doc:"Subscription id (UUID)"`
	TenantID   string    `json:"tenant_id"`
	URL        string    `json:"url"`
	EventTypes []string  `json:"event_types" doc:"Actions this subscription receives; empty means every action"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
}

func toWebhookSubscriptionBody(s domain.WebhookSubscription) WebhookSubscriptionBody {
	eventTypes := s.EventTypes
	if eventTypes == nil {
		eventTypes = []string{}
	}
	return WebhookSubscriptionBody{
		ID:         s.ID,
		TenantID:   s.TenantID,
		URL:        s.URL,
		EventTypes: eventTypes,
		Active:     s.Active,
		CreatedAt:  s.CreatedAt,
	}
}

// IssuedWebhookSubscriptionBody is the JSON shape of a just-created webhook
// subscription: its metadata plus the signing secret, shown exactly once.
// Store it now: only the secret's own value is ever persisted (Task 4.1,
// unlike an api key it is not hashed, since the delivery worker must read it
// back to sign every outbound payload), but it is still never returned
// again by any read after this one response.
type IssuedWebhookSubscriptionBody struct {
	ID         string   `json:"id"`
	TenantID   string   `json:"tenant_id"`
	URL        string   `json:"url"`
	EventTypes []string `json:"event_types"`
	Secret     string   `json:"secret" doc:"HMAC-SHA256 signing secret. Shown once: store it now, it cannot be retrieved again. Verify each delivery's X-Ledger-Signature header as sha256=hex(hmac_sha256(secret, raw_body))."`
}

func toIssuedWebhookSubscriptionBody(secret string, s domain.WebhookSubscription) IssuedWebhookSubscriptionBody {
	eventTypes := s.EventTypes
	if eventTypes == nil {
		eventTypes = []string{}
	}
	return IssuedWebhookSubscriptionBody{
		ID:         s.ID,
		TenantID:   s.TenantID,
		URL:        s.URL,
		EventTypes: eventTypes,
		Secret:     secret,
	}
}

// CreateWebhookSubscriptionInput is the create-subscription request body.
type CreateWebhookSubscriptionInput struct {
	Body struct {
		TenantID   string   `json:"tenant_id" format:"uuid" doc:"Tenant this subscription belongs to"`
		URL        string   `json:"url" doc:"Callback URL; must be an absolute http or https URL"`
		EventTypes []string `json:"event_types,omitempty" doc:"Actions to receive, e.g. transaction.created, transaction.reversed. Omit (or leave empty) to receive every action."`
	}
}

// CreateWebhookSubscriptionOutput wraps a newly created subscription.
type CreateWebhookSubscriptionOutput struct {
	Body IssuedWebhookSubscriptionBody
}

// ListWebhookSubscriptionsInput is the list-webhook-subscriptions request: a
// required tenant filter.
type ListWebhookSubscriptionsInput struct {
	TenantID string `query:"tenant_id" format:"uuid" required:"true" doc:"Tenant to list webhook subscriptions for"`
}

// ListWebhookSubscriptionsOutput is the list-webhook-subscriptions response.
type ListWebhookSubscriptionsOutput struct {
	Body struct {
		Subscriptions []WebhookSubscriptionBody `json:"subscriptions"`
	}
}

type webhookSubscriptionIDInput struct {
	ID string `path:"id" format:"uuid" doc:"Webhook subscription id"`
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
		Description:   "Creates a new tenant with status active. This is the first call in the onboarding flow: create a tenant here, then issue it an api key with POST /v1/admin/keys before any /v1 account or transaction call can use it.",
		Tags:          []string{"admin: tenants"},
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
		Description: "Lists every tenant, regardless of status.",
		Tags:        []string{"admin: tenants"},
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
		Description:   "Sets the tenant's status to active, suspended, or closed. Once a tenant is suspended or closed, every one of its api keys is rejected with 401 on any /v1 call, not only posting: reactivate it (set status back to active) to restore access.",
		Tags:          []string{"admin: tenants"},
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
		OperationID:   "set-tenant-policy",
		Method:        http.MethodPost,
		Path:          "/v1/admin/tenants/{id}/policy",
		Summary:       "Set a tenant's posting guardrails",
		Description:   "Sets the tenant's optional per-currency guardrails (Task 2.4b): a max transaction amount, a daily volume cap, and a currency allowlist. Writes the full policy the body describes; an omitted field clears (unlimits) that guardrail rather than leaving a previous value in place.",
		Tags:          []string{"admin: tenants"},
		DefaultStatus: http.StatusNoContent,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *SetTenantPolicyInput) (*EmptyOutput, error) {
		policy := domain.TenantPolicy{
			MaxTransactionAmount: in.Body.MaxTransactionAmount,
			DailyVolumeLimit:     in.Body.DailyVolumeLimit,
			AllowedCurrencies:    in.Body.AllowedCurrencies,
		}
		if err := deps.Admin.SetTenantPolicy(ctx, in.ID, policy); err != nil {
			return nil, toHumaErr(err)
		}
		return &EmptyOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "shred-tenant-pii",
		Method:      http.MethodPost,
		Path:        "/v1/admin/tenants/{id}/shred-pii",
		Summary:     "Irreversibly erase a tenant's PII encryption key",
		Description: "Crypto-shredding (Task 6.2): destroys the tenant's Data Encryption Key. Every posting description ever " +
			"encrypted under it becomes permanently unreadable (a later read returns a redacted marker, not an error); money data " +
			"(accounts, transactions, amounts, balances) and the tamper-evident audit hash chain are completely untouched and remain " +
			"verifiable. THIS CANNOT BE UNDONE: there is no way to recover an erased description, by anyone, once this call succeeds. " +
			"Requires confirm: true in the body. Idempotent: calling it again for an already-shredded tenant succeeds with no effect.",
		Tags:          []string{"admin: tenants"},
		DefaultStatus: http.StatusNoContent,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *ShredTenantPIIInput) (*EmptyOutput, error) {
		if !in.Body.Confirm {
			return nil, huma.Error400BadRequest("confirm must be true: this operation is irreversible")
		}
		if err := deps.Admin.ShredTenantPII(ctx, in.ID); err != nil {
			return nil, toHumaErr(err)
		}
		return &EmptyOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "issue-key",
		Method:        http.MethodPost,
		Path:          "/v1/admin/keys",
		Summary:       "Issue a new api key",
		Description:   "Mints a new api key for a tenant. The plaintext is returned exactly once, in this response, and is never stored or recoverable again: store it now. This is the second call in the onboarding flow, right after creating a tenant: issue a key with scope post (or admin, for another operator key) before posting accounts or transactions.",
		Tags:          []string{"admin: keys"},
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
		Tags:          []string{"admin: keys"},
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
		Description:   "Immediately and permanently invalidates the key. There is no un-revoke; issue a new key instead.",
		Tags:          []string{"admin: keys"},
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
		Tags:        []string{"admin: keys"},
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

	huma.Register(api, huma.Operation{
		OperationID:   "create-webhook-subscription",
		Method:        http.MethodPost,
		Path:          "/v1/admin/webhooks",
		Summary:       "Create a webhook subscription",
		Description:   "Registers a callback URL to receive signed, retried, at-least-once webhook deliveries for a tenant's posted transactions (Task 4.1). The signing secret is returned exactly once, in this response, and is never stored anywhere recoverable again: store it now to verify each delivery's X-Ledger-Signature header.",
		Tags:          []string{"admin: webhooks"},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *CreateWebhookSubscriptionInput) (*CreateWebhookSubscriptionOutput, error) {
		secret, sub, err := deps.Admin.CreateWebhookSubscription(ctx, in.Body.TenantID, in.Body.URL, in.Body.EventTypes)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &CreateWebhookSubscriptionOutput{Body: toIssuedWebhookSubscriptionBody(secret, sub)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-webhook-subscriptions",
		Method:      http.MethodGet,
		Path:        "/v1/admin/webhooks",
		Summary:     "List a tenant's webhook subscriptions",
		Description: "Lists every subscription for a tenant, active or not. Never includes the signing secret: that is shown once, at creation time, and is never recoverable afterward.",
		Tags:        []string{"admin: webhooks"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *ListWebhookSubscriptionsInput) (*ListWebhookSubscriptionsOutput, error) {
		subs, err := deps.Admin.ListWebhookSubscriptions(ctx, in.TenantID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &ListWebhookSubscriptionsOutput{}
		out.Body.Subscriptions = make([]WebhookSubscriptionBody, 0, len(subs))
		for _, s := range subs {
			out.Body.Subscriptions = append(out.Body.Subscriptions, toWebhookSubscriptionBody(s))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-webhook-subscription",
		Method:        http.MethodDelete,
		Path:          "/v1/admin/webhooks/{id}",
		Summary:       "Delete a webhook subscription",
		Description:   "Deactivates the subscription: the fan-out worker stops creating new pending deliveries for it and the delivery worker stops attempting its existing pending ones, but its delivery history is kept, not discarded.",
		Tags:          []string{"admin: webhooks"},
		DefaultStatus: http.StatusNoContent,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *webhookSubscriptionIDInput) (*EmptyOutput, error) {
		if err := deps.Admin.DeleteWebhookSubscription(ctx, in.ID); err != nil {
			return nil, toHumaErr(err)
		}
		return &EmptyOutput{}, nil
	})
}
