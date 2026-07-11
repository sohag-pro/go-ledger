package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// OpenDisputeInput is the open-dispute request body (Task 6.3, audit A9.2).
type OpenDisputeInput struct {
	Body struct {
		TransactionID string `json:"transaction_id" format:"uuid" doc:"The transaction being disputed; must belong to the caller's tenant"`
		Reason        string `json:"reason" minLength:"1" maxLength:"256" doc:"Free-text reason for the dispute"`
	}
}

// DisputeBody is the JSON shape of a dispute in responses.
type DisputeBody struct {
	ID                      string     `json:"id"`
	TransactionID           string     `json:"transaction_id"`
	Status                  string     `json:"status" doc:"One of: open, resolved_reversed, resolved_rejected"`
	Reason                  string     `json:"reason"`
	ResolutionTransactionID *string    `json:"resolution_transaction_id,omitempty" doc:"The reversal posted on resolution, if any. Only set when status is resolved_reversed."`
	CreatedAt               time.Time  `json:"created_at"`
	ResolvedAt              *time.Time `json:"resolved_at,omitempty" doc:"When the dispute was resolved. Unset while status is open."`
}

func toDisputeBody(d domain.Dispute) DisputeBody {
	return DisputeBody{
		ID:                      d.ID,
		TransactionID:           d.TransactionID,
		Status:                  string(d.Status),
		Reason:                  d.Reason,
		ResolutionTransactionID: d.ResolutionTransactionID,
		CreatedAt:               d.CreatedAt,
		ResolvedAt:              d.ResolvedAt,
	}
}

// DisputeOutput wraps a dispute in a response.
type DisputeOutput struct {
	Body DisputeBody
}

type disputeIDInput struct {
	ID string `path:"id" format:"uuid" doc:"Dispute id"`
}

// ListDisputesInput is the list-disputes request: an optional status filter
// plus keyset paging (Task 6.3, audit A9.2). Status is validated by hand in
// the handler (an empty string means "no filter"), the same convention
// TransactionListInput's Reference field uses, rather than an enum schema
// tag, which cannot express "valid value OR empty".
type ListDisputesInput struct {
	Status string `query:"status" doc:"Optional filter: one of open, resolved_reversed, resolved_rejected. Omitted returns every status."`
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"200" doc:"Max disputes per page"`
	Cursor string `query:"cursor" doc:"Opaque cursor from a previous page's next_cursor"`
}

// ListDisputesOutput is one page of a tenant's disputes.
type ListDisputesOutput struct {
	Body struct {
		Disputes   []DisputeBody `json:"disputes"`
		NextCursor *string       `json:"next_cursor" doc:"Cursor for the next page, or null if this is the last page"`
	}
}

// ResolveDisputeInput is the resolve-dispute request: a path id plus the
// action to take.
type ResolveDisputeInput struct {
	ID   string `path:"id" format:"uuid" doc:"Dispute id"`
	Body struct {
		Action string `json:"action" enum:"reverse,reject" doc:"reverse: post a real reversal of the disputed transaction. reject: no money moves."`
	}
}

func registerDisputes(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "open-dispute",
		Method:        http.MethodPost,
		Path:          "/v1/disputes",
		Summary:       "Open a dispute against a transaction",
		Description:   "Opens a dispute (status open) against transaction_id, which must belong to the caller's tenant. Opening a dispute records intent only; no money moves until it is resolved (POST /v1/disputes/{id}/resolve).",
		Tags:          []string{"disputes"},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *OpenDisputeInput) (*DisputeOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		d, err := deps.Disputes.Open(ctx, tenant, in.Body.TransactionID, in.Body.Reason)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &DisputeOutput{Body: toDisputeBody(d)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-disputes",
		Method:      http.MethodGet,
		Path:        "/v1/disputes",
		Summary:     "List disputes",
		Description: "Lists the tenant's disputes, newest first, optionally filtered by status. Keyset paged: pass the response's next_cursor back as cursor to fetch the next page; next_cursor is null on the last page.",
		Tags:        []string{"disputes"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *ListDisputesInput) (*ListDisputesOutput, error) {
		var status *domain.DisputeStatus
		if in.Status != "" {
			s := domain.DisputeStatus(in.Status)
			if !s.Valid() {
				return nil, huma.Error422UnprocessableEntity("status must be one of: open, resolved_reversed, resolved_rejected")
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
		rows, err := deps.Disputes.List(ctx, tenant, status, after, in.Limit+1)
		if err != nil {
			return nil, toHumaErr(err)
		}
		hasMore := len(rows) > in.Limit
		if hasMore {
			rows = rows[:in.Limit]
		}
		out := &ListDisputesOutput{}
		out.Body.Disputes = make([]DisputeBody, 0, len(rows))
		for _, d := range rows {
			out.Body.Disputes = append(out.Body.Disputes, toDisputeBody(d))
		}
		if hasMore {
			last := rows[len(rows)-1]
			c := encodeCursor(last.CreatedAt, last.ID)
			out.Body.NextCursor = &c
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-dispute",
		Method:      http.MethodGet,
		Path:        "/v1/disputes/{id}",
		Summary:     "Get a dispute",
		Description: "Fetches a single dispute by id, including its resolution details once resolved.",
		Tags:        []string{"disputes"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *disputeIDInput) (*DisputeOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		d, err := deps.Disputes.Get(ctx, tenant, in.ID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &DisputeOutput{Body: toDisputeBody(d)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:  "resolve-dispute",
		Method:       http.MethodPost,
		Path:         "/v1/disputes/{id}/resolve",
		Summary:      "Resolve a dispute",
		Description:  "Resolves the dispute named by id. action=reverse posts a real reversal of the disputed transaction through the normal posting path (screening, policy, account status, encryption), sets resolution_transaction_id to the reversal's id, and moves status to resolved_reversed; if the transaction was already reversed by other means, the existing reversal is reused rather than posting a second one. action=reject moves status to resolved_rejected with no money movement. Resolving an already-resolved dispute returns 409 Conflict.",
		Tags:         []string{"disputes"},
		MaxBodyBytes: MaxRequestBodyBytes,
		Security:     bearerSecurity,
	}, func(ctx context.Context, in *ResolveDisputeInput) (*DisputeOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		d, err := deps.Disputes.Resolve(ctx, tenant, in.ID, in.Body.Action)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &DisputeOutput{Body: toDisputeBody(d)}, nil
	})
}
