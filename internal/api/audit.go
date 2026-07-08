package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// AuditEntryBody is one audit row in a response. Before and After are raw JSON
// (Before is omitted for a create). Amounts inside After are minor units.
type AuditEntryBody struct {
	ID            string    `json:"id"`
	Action        string    `json:"action"`
	TransactionID string    `json:"transaction_id"`
	Actor         string    `json:"actor"`
	Before        any       `json:"before,omitempty"`
	After         any       `json:"after"`
	CreatedAt     time.Time `json:"created_at"`
}

// AuditListOutput is a list of audit entries.
type AuditListOutput struct {
	Body struct {
		Entries []AuditEntryBody `json:"entries"`
	}
}

// AccountAuditInput is the account audit request: a path id plus keyset paging.
type AccountAuditInput struct {
	ID     string `path:"id" format:"uuid" doc:"Account id"`
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"200" doc:"Max entries per page"`
	Cursor string `query:"cursor" doc:"Opaque cursor from a previous page's next_cursor"`
}

// AccountAuditOutput is one page of an account's audit log.
type AccountAuditOutput struct {
	Body struct {
		Entries    []AuditEntryBody `json:"entries"`
		NextCursor *string          `json:"next_cursor" doc:"Cursor for the next page, or null if this is the last page"`
	}
}

func toAuditBody(e domain.AuditEntry) AuditEntryBody {
	b := AuditEntryBody{
		ID:            e.ID,
		Action:        e.Action,
		TransactionID: e.TransactionID,
		Actor:         e.Actor,
		After:         e.After,
		CreatedAt:     e.CreatedAt,
	}
	if len(e.Before) > 0 {
		b.Before = e.Before
	}
	return b
}

// VerifyAuditOutput is the result of walking the caller's tenant audit chain
// (ADR-012, "A per-tenant, tamper-evident audit chain"). FirstBreakID is null
// when Valid is true.
type VerifyAuditOutput struct {
	Body struct {
		Valid        bool    `json:"valid"`
		Checked      int     `json:"checked"`
		FirstBreakID *string `json:"first_break_id" doc:"Id of the first audit row that failed to verify, or null if the chain is intact"`
	}
}

func registerAudit(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "get-transaction-audit",
		Method:      http.MethodGet,
		Path:        "/v1/transactions/{id}/audit",
		Summary:     "List a transaction's audit log",
		Tags:        []string{"transactions"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *transactionIDInput) (*AuditListOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		entries, err := deps.Audit.ByTransaction(ctx, tenant, in.ID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return auditOutput(entries), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-account-audit",
		Method:      http.MethodGet,
		Path:        "/v1/accounts/{id}/audit",
		Summary:     "List an account's audit log",
		Tags:        []string{"accounts"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *AccountAuditInput) (*AccountAuditOutput, error) {
		after, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		entries, err := deps.Audit.ByAccount(ctx, tenant, in.ID, after, in.Limit)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &AccountAuditOutput{}
		out.Body.Entries = make([]AuditEntryBody, 0, len(entries))
		for _, e := range entries {
			out.Body.Entries = append(out.Body.Entries, toAuditBody(e))
		}
		// A full page implies there may be more; hand back a cursor at the last
		// entry. A short page is the end, so next_cursor stays null.
		if in.Limit > 0 && len(entries) == in.Limit {
			last := entries[len(entries)-1]
			c := encodeCursor(last.CreatedAt, last.ID)
			out.Body.NextCursor = &c
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "verify-audit-chain",
		Method:      http.MethodGet,
		Path:        "/v1/audit/verify",
		Summary:     "Verify the tamper-evident audit chain",
		Description: "Walks the caller's tenant audit chain oldest first and recomputes every row's hash, " +
			"detecting any row that was altered after it was written (ADR-012).",
		Tags:     []string{"audit"},
		Security: bearerSecurity,
	}, func(ctx context.Context, _ *struct{}) (*VerifyAuditOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		result, err := deps.Audit.Verify(ctx, tenant)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &VerifyAuditOutput{}
		out.Body.Valid = result.Valid
		out.Body.Checked = result.Checked
		if result.FirstBreakID != "" {
			out.Body.FirstBreakID = &result.FirstBreakID
		}
		return out, nil
	})
}

func auditOutput(entries []domain.AuditEntry) *AuditListOutput {
	out := &AuditListOutput{}
	out.Body.Entries = make([]AuditEntryBody, 0, len(entries))
	for _, e := range entries {
		out.Body.Entries = append(out.Body.Entries, toAuditBody(e))
	}
	return out
}
