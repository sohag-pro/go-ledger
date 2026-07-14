package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// PendingBody is the JSON shape of a pending (over-threshold, held)
// transaction in responses (ADR-025). Payload itself is never exposed: it is
// an internal replay format (postPayloadBody/convertPayloadBody/
// reversePayloadBody in internal/ledger), not a stable public contract.
type PendingBody struct {
	ID   string `json:"id"`
	Kind string `json:"kind" doc:"Which write path this pending will replay on approval: post, convert, or reverse"`
	// Status is the pending's own lifecycle state, not to be confused with
	// the output struct's Status field elsewhere in this package (which sets
	// the HTTP response code): this is a JSON body field.
	Status            string     `json:"status" doc:"One of: pending, approved, rejected, cancelled, expired"`
	ThresholdCurrency string     `json:"threshold_currency" doc:"The currency whose configured threshold this transaction exceeded"`
	ThresholdAmount   int64      `json:"threshold_amount" doc:"The configured threshold amount, in minor units, this transaction exceeded"`
	CreatedBy         string     `json:"created_by"`
	CreatedAt         time.Time  `json:"created_at"`
	DecidedBy         *string    `json:"decided_by,omitempty" doc:"Set once a decision (approve, reject, cancel, or the TTL sweep) has been made"`
	DecidedAt         *time.Time `json:"decided_at,omitempty"`
	Reason            *string    `json:"reason,omitempty" doc:"Set when status is rejected"`
	TransactionID     *string    `json:"transaction_id,omitempty" doc:"Set when status is approved: the transaction the approval posted"`
}

func toPendingBody(p *domain.PendingTransaction) PendingBody {
	return PendingBody{
		ID:                p.ID,
		Kind:              string(p.Kind),
		Status:            string(p.Status),
		ThresholdCurrency: p.ThresholdCcy,
		ThresholdAmount:   p.ThresholdAmt,
		CreatedBy:         p.CreatedBy,
		CreatedAt:         p.CreatedAt,
		DecidedBy:         p.DecidedBy,
		DecidedAt:         p.DecidedAt,
		Reason:            p.Reason,
		TransactionID:     p.TransactionID,
	}
}

// PendingOutput wraps a single pending transaction in a response.
type PendingOutput struct {
	Body PendingBody
}

// pendingIDInput is the path-id-only request shared by get, approve, and
// cancel (reject adds a body, see RejectPendingInput).
type pendingIDInput struct {
	ID string `path:"id" format:"uuid" doc:"Pending transaction id"`
}

// PendingListInput is the list-pending request: an optional status filter,
// validated by hand the same way ListDisputesInput's Status is (an empty
// string means "no filter", which an enum schema tag cannot express), plus
// keyset paging.
type PendingListInput struct {
	Status string `query:"status" doc:"Optional filter: one of pending, approved, rejected, cancelled, expired. Omitted returns every status."`
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"200" doc:"Max pending transactions per page"`
	Cursor string `query:"cursor" doc:"Opaque cursor from a previous page's next_cursor"`
}

// PendingListOutput is one page of the tenant's pending transactions.
type PendingListOutput struct {
	Body struct {
		Pending    []PendingBody `json:"pending"`
		NextCursor *string       `json:"next_cursor" doc:"Cursor for the next page, or null if this is the last page"`
	}
}

// RejectPendingInput is the reject request: a path id plus an optional
// free-text reason recorded on the pending.
type RejectPendingInput struct {
	ID   string `path:"id" format:"uuid" doc:"Pending transaction id"`
	Body struct {
		Reason *string `json:"reason,omitempty" maxLength:"256" doc:"Optional free-text reason recorded on the pending"`
	}
}

// ApprovePendingOutput is the approve response: the transaction the approval
// posted. Approving an already-approved pending is idempotent (ApprovalService.
// Approve's own doc comment) and returns the SAME transaction rather than
// posting a second one, still with status 200.
type ApprovePendingOutput struct {
	Body TransactionBody
}

func registerApprovals(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "list-pending",
		Method:      http.MethodGet,
		Path:        "/v1/pending",
		Summary:     "List pending (over-threshold) transactions",
		Description: "Lists the tenant's held transactions awaiting an approval decision (ADR-025), newest first, optionally filtered by status. Keyset paged: pass the response's next_cursor back as cursor to fetch the next page; next_cursor is null on the last page.",
		Tags:        []string{"approvals"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *PendingListInput) (*PendingListOutput, error) {
		var status *domain.PendingStatus
		if in.Status != "" {
			s := domain.PendingStatus(in.Status)
			if !s.Valid() {
				return nil, huma.Error422UnprocessableEntity("status must be one of: pending, approved, rejected, cancelled, expired")
			}
			status = &s
		}
		after, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		// Requested as limit+1, mirroring list-disputes and list-transactions:
		// getting one row more than asked for is how hasMore below detects a
		// next page exists without a second round trip.
		rows, err := deps.Approvals.List(ctx, tenant, status, after, in.Limit+1)
		if err != nil {
			return nil, toHumaErr(err)
		}
		hasMore := len(rows) > in.Limit
		if hasMore {
			rows = rows[:in.Limit]
		}
		out := &PendingListOutput{}
		out.Body.Pending = make([]PendingBody, 0, len(rows))
		for i := range rows {
			out.Body.Pending = append(out.Body.Pending, toPendingBody(&rows[i]))
		}
		if hasMore {
			last := rows[len(rows)-1]
			c := encodeCursor(last.CreatedAt, last.ID)
			out.Body.NextCursor = &c
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-pending",
		Method:      http.MethodGet,
		Path:        "/v1/pending/{id}",
		Summary:     "Get a pending transaction",
		Tags:        []string{"approvals"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *pendingIDInput) (*PendingOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		p, err := deps.Approvals.Get(ctx, tenant, in.ID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &PendingOutput{Body: toPendingBody(p)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "approve-pending",
		Method:      http.MethodPost,
		Path:        "/v1/pending/{id}/approve",
		Summary:     "Approve a pending transaction",
		Description: "Approves the pending transaction named by id and replays it through the normal posting path against CURRENT balances (ADR-025): a plain post, an FX conversion, or a reversal, whichever it originally was. Requires approve scope (or admin). Approving an already-approved pending is idempotent and returns the SAME transaction rather than posting a second one; approving one already rejected, cancelled, or expired returns 409 Conflict.",
		Tags:        []string{"approvals"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *pendingIDInput) (*ApprovePendingOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		// The actor is the individual API key behind the request, not the
		// tenant. A held pending's CreatedBy is stamped with the creating key
		// the same way (holdForApproval via actorFromCtx), so with
		// ApprovalConfig.RequireDifferentActor enabled a key cannot approve a
		// pending it created: maker-checker compares two distinct keys, which
		// per-tenant granularity could never do. In demo the flag is off, so a
		// single key may create and approve for demonstration.
		txn, err := deps.Approvals.Approve(ctx, tenant, in.ID, actorFromCtx(ctx, tenant))
		if err != nil {
			return nil, toApprovalHumaErr(err)
		}
		return &ApprovePendingOutput{Body: toTransactionBody(*txn)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:  "reject-pending",
		Method:       http.MethodPost,
		Path:         "/v1/pending/{id}/reject",
		Summary:      "Reject a pending transaction",
		Description:  "Rejects the pending transaction named by id (ADR-025): no money ever moves for it. Requires approve scope (or admin). Rejecting an already-decided pending (approved, rejected, cancelled, or expired) returns 409 Conflict.",
		Tags:         []string{"approvals"},
		MaxBodyBytes: MaxRequestBodyBytes,
		Security:     bearerSecurity,
	}, func(ctx context.Context, in *RejectPendingInput) (*PendingOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		if err := deps.Approvals.Reject(ctx, tenant, in.ID, actorFromCtx(ctx, tenant), in.Body.Reason); err != nil {
			return nil, toApprovalHumaErr(err)
		}
		p, err := deps.Approvals.Get(ctx, tenant, in.ID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &PendingOutput{Body: toPendingBody(p)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "cancel-pending",
		Method:      http.MethodPost,
		Path:        "/v1/pending/{id}/cancel",
		Summary:     "Cancel a pending transaction",
		Description: "Cancels the pending transaction named by id (ADR-025): only the tenant that created it may cancel, and only while it is still pending (returns 403 Forbidden otherwise for a non-creator, or 409 Conflict for one already decided). No money ever moves for it. Cancel requires only post scope, the creator's own key, unlike approve/reject which require approve scope.",
		Tags:        []string{"approvals"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *pendingIDInput) (*PendingOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		if err := deps.Approvals.Cancel(ctx, tenant, in.ID, actorFromCtx(ctx, tenant)); err != nil {
			return nil, toApprovalHumaErr(err)
		}
		p, err := deps.Approvals.Get(ctx, tenant, in.ID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &PendingOutput{Body: toPendingBody(p)}, nil
	})
}

// toApprovalHumaErr maps the approval decision sentinels (ADR-025) to their
// HTTP status before falling back to the shared toHumaErr for everything
// else (including domain.ErrPendingTransactionNotFound, handled there
// alongside every other "no such resource" case). An already-decided pending
// (ErrPendingAlreadyDecided) and a four-eyes violation (ErrCannotApproveOwn)
// are both 409 Conflict: the request is well-formed, it just cannot be
// satisfied against the pending's current state or by this same actor. A
// non-creator's cancel attempt (ErrNotPendingCreator) is 403 Forbidden, the
// same class TenantNotActiveError already uses for "valid request, wrong
// caller."
func toApprovalHumaErr(err error) error {
	switch {
	case errors.Is(err, domain.ErrPendingAlreadyDecided):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, domain.ErrCannotApproveOwn):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, domain.ErrNotPendingCreator):
		return huma.Error403Forbidden(err.Error())
	default:
		return toHumaErr(err)
	}
}
