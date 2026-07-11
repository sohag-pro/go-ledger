package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
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

// AuditHeadBody is the tenant's current chain head (Task 5.3): the chain_seq
// and row_hash of its latest audit_log row.
type AuditHeadBody struct {
	ChainSeq int64  `json:"chain_seq"`
	RowHash  string `json:"row_hash"`
}

// AuditAnchorBody is the tenant's most recently recorded off-box anchor
// (Task 5.3, migration 0025): the chain_seq and row_hash the anchor job read
// and logged at CreatedAt.
type AuditAnchorBody struct {
	ChainSeq  int64     `json:"chain_seq"`
	RowHash   string    `json:"row_hash"`
	CreatedAt time.Time `json:"created_at"`
}

// VerifyAuditInput is the verify-audit-chain request: no path parameters, and
// one optional query flag choosing which of AuditService's two walks runs.
type VerifyAuditInput struct {
	Fast bool `query:"fast" doc:"When true, verify only from the tenant's latest off-box anchor (bounded cost) instead of from genesis (complete but O(chain length)). Falls back to a full verify when the tenant has no anchor yet. See the response's anchor field and ADR-012's Task 5.3 addendum for the trust model this trades: the anchored prefix is trusted as a checkpoint, not re-proven."`
}

// VerifyAuditOutput is the result of walking the caller's tenant audit chain
// (ADR-012, "A per-tenant, tamper-evident audit chain"). FirstBreakID is null
// when Valid is true. Pending is the number of posted events not yet
// reflected in the chain (ADR-017): the chain is built asynchronously by a
// single background chainer, so a just-posted transaction can briefly show
// up here before its audit-chain link exists.
//
// Head and Anchor (Task 5.3, audit A2.4) are independent of which walk
// (fast=true or false) actually ran: Head is always the tenant's current
// live chain head, and Anchor is always the latest off-box-logged
// checkpoint, or null if the tenant has none yet. A caller (or an automated
// external monitor comparing Head against the same anchor line its own log
// shipper captured, per Task 5.6) can tell a rewrite of already-anchored
// history apart from ordinary chain growth: if Head.ChainSeq equals
// Anchor.ChainSeq but Head.RowHash does not equal Anchor.RowHash, the
// anchored row itself was altered after being anchored, something a
// self-consistent in-database rewrite would not otherwise reveal (see
// migration 0025's own doc comment).
type VerifyAuditOutput struct {
	Body struct {
		Valid        bool             `json:"valid"`
		Checked      int              `json:"checked"`
		FirstBreakID *string          `json:"first_break_id" doc:"Id of the first audit row that failed to verify, or null if the chain is intact"`
		Pending      int              `json:"pending" doc:"Posted events not yet reflected in the chain (ADR-017): the chain is built asynchronously, so this lags briefly behind posting"`
		Head         *AuditHeadBody   `json:"head" doc:"The tenant's current chain head (chain_seq + row_hash), or null for an empty chain"`
		Anchor       *AuditAnchorBody `json:"anchor" doc:"The tenant's most recently recorded off-box anchor, or null if none has been recorded yet"`
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
			"detecting any row that was altered after it was written (ADR-012). The chain itself is built " +
			"asynchronously by a single background chainer (ADR-017), so `pending` reports how many posted " +
			"events are not yet reflected in it; a nonzero, non-growing pending count is normal lag, not a fault.",
		Tags:     []string{"audit"},
		Security: bearerSecurity,
	}, func(ctx context.Context, in *VerifyAuditInput) (*VerifyAuditOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		var result ledger.VerifyResult
		if in.Fast {
			result, err = deps.Audit.VerifyFromLatestAnchor(ctx, tenant)
		} else {
			result, err = deps.Audit.Verify(ctx, tenant)
		}
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &VerifyAuditOutput{}
		out.Body.Valid = result.Valid
		out.Body.Checked = result.Checked
		out.Body.Pending = result.Pending
		if result.FirstBreakID != "" {
			out.Body.FirstBreakID = &result.FirstBreakID
		}

		if chainSeq, rowHash, ok, err := deps.Audit.Head(ctx, tenant); err != nil {
			return nil, toHumaErr(err)
		} else if ok {
			out.Body.Head = &AuditHeadBody{ChainSeq: chainSeq, RowHash: rowHash}
		}
		if anchor, ok, err := deps.Audit.LatestAnchor(ctx, tenant); err != nil {
			return nil, toHumaErr(err)
		} else if ok {
			out.Body.Anchor = &AuditAnchorBody{ChainSeq: anchor.ChainSeq, RowHash: anchor.RowHash, CreatedAt: anchor.CreatedAt}
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
