package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// PostingInput is one leg of a transaction in the request body.
type PostingInput struct {
	AccountID   string `json:"account_id" format:"uuid" doc:"Account this leg posts to"`
	Amount      int64  `json:"amount" doc:"Signed amount in minor units: positive debit, negative credit"`
	Description string `json:"description,omitempty" maxLength:"256" doc:"Optional narration for this line"`
}

// PostingBody is one leg of a transaction in responses. Currency is the
// posting's own currency (ADR-014): a cross-currency (FX) transaction spans
// two currencies, so each leg is labeled with its own rather than inheriting
// one shared transaction-level currency.
type PostingBody struct {
	AccountID   string `json:"account_id"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
}

// TransactionBody is the JSON shape of a transaction in responses. There is
// deliberately no transaction-level currency field (ADR-014 decision 1): a
// convert transaction spans two currencies, so a single top-level currency
// could never label every posting correctly. Each PostingBody carries its
// own currency instead.
type TransactionBody struct {
	ID       string        `json:"id"`
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
			Currency:    string(p.Amount.Currency()),
			Description: p.Description,
		})
	}
	return TransactionBody{ID: t.ID, Postings: postings}
}

// FXDetailBody is the applied-rate detail for a cross-currency convert: the
// requested and converted amounts, the mid rate and spread applied, and
// where that rate came from (ADR-014 decision 7). It only appears on the
// convert response: GET /v1/transactions/{id} reports per-posting currency
// only, not the fx_* snapshot (ADR-014, "Out of scope for v1").
type FXDetailBody struct {
	SourceAmount    int64     `json:"source_amount" doc:"Amount converted, in minor units of the source account's currency"`
	ConvertedAmount int64     `json:"converted_amount" doc:"Amount credited, in minor units of the destination account's currency"`
	MidRateE8       int64     `json:"mid_rate_e8" doc:"Mid rate (destination units per source unit), scaled by 1e8, before the spread"`
	AppliedE8       int64     `json:"applied_e8" doc:"Informational, spread-adjusted rate actually reflected in converted_amount"`
	SpreadBps       int32     `json:"spread_bps" doc:"Spread widened against the customer, in basis points"`
	RateSource      string    `json:"rate_source" doc:"Where the mid rate came from"`
	EffectiveAt     time.Time `json:"effective_at" doc:"When the applied fx_rates row went live"`
	RateID          int64     `json:"rate_id" doc:"The fx_rates row id the mid rate and spread were read from"`
}

func toFXDetailBody(fx *domain.FXDetail) FXDetailBody {
	if fx == nil {
		return FXDetailBody{}
	}
	return FXDetailBody{
		SourceAmount:    fx.SourceAmount,
		ConvertedAmount: fx.ConvertedAmount,
		MidRateE8:       fx.MidRateE8,
		AppliedE8:       fx.AppliedE8,
		SpreadBps:       fx.SpreadBps,
		RateSource:      fx.RateSource,
		EffectiveAt:     fx.EffectiveAt,
		RateID:          fx.RateID,
	}
}

// ConvertInput is the convert request body: the account to debit, the
// account to credit, and the amount to convert, in the from account's own
// currency. The destination currency always comes from the to account
// (ADR-014 decision 3): a client cannot supply a rate or target currency
// directly, since a client-controlled rate would be a money-theft vector.
type ConvertInput struct {
	// See CreateTransactionInput's IdempotencyKey field for why this is not
	// required:"true": a missing header should fail our explicit 400 check
	// below with a clear message, not huma's generic 422.
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"255" doc:"Required. Retrying with the same key returns the original conversion; reusing a key with a different body returns 409."`
	Body           struct {
		FromAccount  string `json:"from_account" format:"uuid" doc:"Account to debit, in its own currency"`
		ToAccount    string `json:"to_account" format:"uuid" doc:"Account to credit; its currency is the conversion's destination currency"`
		SourceAmount int64  `json:"source_amount" doc:"Amount to convert, in minor units of the from account's currency; must be positive"`
	}
}

// ConvertResponseBody is the JSON shape of a successful convert: the posted
// transaction (source account, source clearing, destination clearing,
// destination account) plus the FX rate detail actually applied.
type ConvertResponseBody struct {
	Transaction TransactionBody `json:"transaction"`
	FX          FXDetailBody    `json:"fx"`
}

// ConvertOutput is the convert response. Replayed mirrors
// CreateTransactionOutput: true when an Idempotency-Key matched an earlier
// convert request and the original conversion was returned unchanged.
type ConvertOutput struct {
	Replayed bool `header:"Idempotent-Replayed"`
	Body     ConvertResponseBody
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
		Security:      bearerSecurity,
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
		Security:    bearerSecurity,
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

	huma.Register(api, huma.Operation{
		OperationID:   "convert-transaction",
		Method:        http.MethodPost,
		Path:          "/v1/transactions/convert",
		Summary:       "Convert between currencies",
		Description:   "Converts source_amount from the from_account's currency into the to_account's currency, at the tenant's current FX rate, and posts the four resulting legs (debit the from account, credit its currency's clearing account, debit the to currency's clearing account, credit the to account) atomically. The destination currency always comes from the to account: a client cannot supply a rate or target currency directly. Requires an Idempotency-Key header, the same as POST /v1/transactions: a repeat with the same key returns the original conversion (with Idempotent-Replayed: true) instead of converting again, and reusing a key with a different body returns 409 Conflict.",
		Tags:          []string{"transactions"},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *ConvertInput) (*ConvertOutput, error) {
		if in.IdempotencyKey == "" {
			return nil, huma.Error400BadRequest("Idempotency-Key header is required")
		}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		req := ledger.ConvertRequest{
			FromAccountID: in.Body.FromAccount,
			ToAccountID:   in.Body.ToAccount,
			SourceAmount:  in.Body.SourceAmount,
		}
		idem := &domain.Idempotency{Key: in.IdempotencyKey}
		txn, replayed, err := deps.Transactions.Convert(ctx, tenant, req, idem)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &ConvertOutput{
			Replayed: replayed,
			Body: ConvertResponseBody{
				Transaction: toTransactionBody(*txn),
				FX:          toFXDetailBody(txn.FX),
			},
		}, nil
	})
}
