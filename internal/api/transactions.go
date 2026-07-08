package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// PostingInput is one leg of a transaction in the request body.
type PostingInput struct {
	AccountID   string `json:"account_id" format:"uuid" doc:"Account this leg posts to"`
	Amount      int64  `json:"amount" doc:"Signed amount in minor units: positive debit, negative credit"`
	Description string `json:"description,omitempty" maxLength:"256" doc:"Optional narration for this line"`
}

// PostingBody is one leg of a transaction in responses.
type PostingBody struct {
	AccountID   string `json:"account_id"`
	Amount      int64  `json:"amount"`
	Description string `json:"description"`
}

// TransactionBody is the JSON shape of a transaction in responses.
type TransactionBody struct {
	ID       string        `json:"id"`
	Currency string        `json:"currency"`
	Postings []PostingBody `json:"postings"`
}

// CreateTransactionInput is the post-transaction request body.
type CreateTransactionInput struct {
	// The schema field is left optional (not required:"true") so a missing
	// header fails our explicit 400 check below with a clear message,
	// rather than huma's generic schema-validation 422.
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"255" doc:"Required. Retrying with the same key returns the original transaction; reusing a key with a different body returns 409."`
	Body           struct {
		Currency string         `json:"currency" pattern:"^[A-Z]{3}$" doc:"ISO 4217 code shared by every posting"`
		Postings []PostingInput `json:"postings" minItems:"2" maxItems:"100" doc:"Two or more legs that must sum to zero"`
	}
}

// TransactionOutput wraps a transaction in a response.
type TransactionOutput struct {
	Body TransactionBody
}

// CreateTransactionOutput is the post-transaction response. Replayed is surfaced
// as the Idempotent-Replayed header: true when an Idempotency-Key matched an
// earlier request and the original transaction was returned unchanged.
type CreateTransactionOutput struct {
	Replayed bool `header:"Idempotent-Replayed"`
	Body     TransactionBody
}

type transactionIDInput struct {
	ID string `path:"id" format:"uuid" doc:"Transaction id"`
}

func toTransactionBody(t domain.Transaction) TransactionBody {
	postings := make([]PostingBody, 0, len(t.Postings))
	for _, p := range t.Postings {
		postings = append(postings, PostingBody{
			AccountID:   p.AccountID,
			Amount:      p.Amount.Amount(),
			Description: p.Description,
		})
	}
	return TransactionBody{ID: t.ID, Currency: currencyOf(t), Postings: postings}
}

// currencyOf returns the transaction's currency, taken from its first posting
// (Validate guarantees they all share one). Empty for a transaction with no
// postings, which the API never produces.
func currencyOf(t domain.Transaction) string {
	if len(t.Postings) == 0 {
		return ""
	}
	return string(t.Postings[0].Amount.Currency())
}

func registerTransactions(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "create-transaction",
		Method:        http.MethodPost,
		Path:          "/v1/transactions",
		Summary:       "Post a transaction",
		Description:   "Posts a balanced transaction whose postings sum to zero in the given currency. Requires an Idempotency-Key header to make retries safe: a repeat with the same key returns the original transaction (with Idempotent-Replayed: true) instead of posting again, and reusing a key with a different body returns 409 Conflict.",
		Tags:          []string{"transactions"},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxRequestBodyBytes,
	}, func(ctx context.Context, in *CreateTransactionInput) (*CreateTransactionOutput, error) {
		if in.IdempotencyKey == "" {
			return nil, huma.Error400BadRequest("Idempotency-Key header is required")
		}
		currency := domain.Currency(in.Body.Currency)
		postings := make([]domain.Posting, 0, len(in.Body.Postings))
		for _, p := range in.Body.Postings {
			amount, err := domain.NewMoney(p.Amount, currency)
			if err != nil {
				return nil, toHumaErr(err)
			}
			postings = append(postings, domain.Posting{
				AccountID:   p.AccountID,
				Amount:      amount,
				Description: p.Description,
			})
		}
		txn := &domain.Transaction{Postings: postings}
		idem := &domain.Idempotency{Key: in.IdempotencyKey}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		replayed, err := deps.Transactions.Post(ctx, tenant, txn, idem)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &CreateTransactionOutput{Replayed: replayed, Body: toTransactionBody(*txn)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-transaction",
		Method:      http.MethodGet,
		Path:        "/v1/transactions/{id}",
		Summary:     "Get a transaction",
		Tags:        []string{"transactions"},
	}, func(ctx context.Context, in *transactionIDInput) (*TransactionOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		txn, err := deps.Transactions.Get(ctx, tenant, in.ID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &TransactionOutput{Body: toTransactionBody(txn)}, nil
	})
}
