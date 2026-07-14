package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/paging"
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
	// ReversesTransactionID is set only when this transaction is itself a
	// reversal (Task 4.2, audit A1.2): the id of the transaction it reverses.
	// Omitted for an ordinary post or convert.
	ReversesTransactionID *string `json:"reverses_transaction_id,omitempty" doc:"Set only when this transaction is a reversal: the id of the transaction it reverses"`
	// Reference is the caller's optional external id for reconciliation
	// (Task 4.3, audit A1.3), unique per tenant when present. Omitted when
	// the transaction was posted without one.
	Reference *string `json:"reference,omitempty" doc:"Optional client-supplied external reference, unique per tenant"`
	// EffectiveAt is the value date (Task 4.3, audit A1.3): when the
	// transaction is considered to have happened economically. Always
	// present in a response, even when the caller supplied none: it then
	// falls back to when the transaction was actually posted.
	EffectiveAt time.Time `json:"effective_at" doc:"Value date; defaults to when the transaction was posted if not supplied"`
}

// CreateTransactionInput is the post-transaction request body.
type CreateTransactionInput struct {
	// The schema field is left optional (not required:"true") so a missing
	// header fails our explicit 400 check below with a clear message,
	// rather than huma's generic schema-validation 422.
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"255" doc:"Required. Retrying with the same key returns the original transaction; reusing a key with a different body returns 409. Keys are remembered for a bounded replay window (server-configured IDEMPOTENCY_TTL, default 24h): once a key expires it is treated as unused, so reusing it after the window starts a brand-new transaction instead of replaying or conflicting."`
	Body           struct {
		Currency string         `json:"currency" pattern:"^[A-Z]{3}$" doc:"ISO 4217 code shared by every posting"`
		Postings []PostingInput `json:"postings" minItems:"2" maxItems:"100" doc:"Two or more legs that must sum to zero"`
		// Reference is an optional external id for reconciliation (Task 4.3,
		// audit A1.3), for example an upstream payment processor's charge id
		// or a bank statement line. Must be unique for this tenant when
		// supplied: reusing one already in use returns 409 Conflict.
		Reference *string `json:"reference,omitempty" maxLength:"256" doc:"Optional external reference for reconciliation, unique per tenant. Reusing one already in use returns 409."`
		// EffectiveAt is an optional value date (Task 4.3, audit A1.3),
		// distinct from when the row is actually written. Omitted, it
		// defaults to the post time.
		EffectiveAt *time.Time `json:"effective_at,omitempty" doc:"Optional value date, distinct from when the transaction was posted. Defaults to the post time when omitted."`
	}
}

// TransactionOutput wraps a transaction in a response.
type TransactionOutput struct {
	Body TransactionBody
}

// CreateTransactionOutput is the post-transaction response. Replayed is surfaced
// as the Idempotent-Replayed header: true when an Idempotency-Key matched an
// earlier request and the original transaction was returned unchanged.
//
// Status overrides the operation's DefaultStatus (201) at runtime: huma reads
// a field literally named Status on an output struct and uses it as the
// actual response code instead (see huma's processOutputType), so every
// return path through create-transaction sets it explicitly, either 201 (the
// transaction posted) or 202 (ADR-025: postings exceeded the tenant's
// approval threshold and were held as a pending instead; Body.Pending is set
// in that case and the TransactionBody fields are left zero).
type CreateTransactionOutput struct {
	Status   int
	Replayed bool `header:"Idempotent-Replayed"`
	Body     struct {
		TransactionBody
		Pending *PendingBody `json:"pending,omitempty" doc:"Set instead of the transaction fields when postings exceeded the approval threshold (ADR-025) and the request was held for approval rather than posted. Poll GET /v1/pending/{id} for its decision."`
	}
}

type transactionIDInput struct {
	ID string `path:"id" format:"uuid" doc:"Transaction id"`
}

// ReverseTransactionOutput is the reverse-transaction response. AlreadyReversed
// mirrors CreateTransactionOutput's Replayed / ConvertOutput's Replayed: true
// when the original already had a reversal and the existing one was returned
// unchanged instead of a new one being posted. The response status is 201
// whenever a reversal exists (new or already-reversed), or 202 (ADR-025) when
// the reversal itself exceeded the approval threshold and was held instead;
// see CreateTransactionOutput's Status doc comment for the mechanism.
type ReverseTransactionOutput struct {
	Status          int
	AlreadyReversed bool `header:"Already-Reversed"`
	Body            struct {
		TransactionBody
		Pending *PendingBody `json:"pending,omitempty" doc:"Set instead of the transaction fields when the reversal exceeded the approval threshold (ADR-025) and the request was held for approval rather than posted."`
	}
}

// TransactionListInput is the list-transactions request: optional date-range
// and reference filters, plus keyset paging (Task 4.4, audit A7.2). From and
// To follow the same half-open convention as domain.TransactionFilter: From
// is inclusive, To is exclusive.
type TransactionListInput struct {
	From          string `query:"from" doc:"RFC3339 timestamp. Only transactions created at or after this time."`
	To            string `query:"to" doc:"RFC3339 timestamp. Only transactions created strictly before this time."`
	EffectiveFrom string `query:"effective_from" doc:"RFC3339 timestamp. Only transactions whose value date (effective_at, falling back to created_at when unset) is at or after this time."`
	EffectiveTo   string `query:"effective_to" doc:"RFC3339 timestamp. Only transactions whose value date (effective_at, falling back to created_at when unset) is strictly before this time."`
	Reference     string `query:"reference" doc:"Exact match on the transaction's reference."`
	Limit         int    `query:"limit" default:"50" minimum:"1" maximum:"200" doc:"Max transactions per page"`
	Cursor        string `query:"cursor" doc:"Opaque cursor from a previous page's next_cursor"`
}

// TransactionListOutput is one page of a tenant's transactions.
type TransactionListOutput struct {
	Body struct {
		Transactions []TransactionBody `json:"transactions"`
		NextCursor   *string           `json:"next_cursor" doc:"Cursor for the next page, or null if this is the last page"`
	}
}

// TransactionExportInput is the export-transactions request: the same
// from/to/reference filters as TransactionListInput, plus format (Task 4.4,
// audit A7.2). Unlike the list endpoint this is not paged: it returns every
// matching transaction up to ledger.MaxExportRows in a single response.
type TransactionExportInput struct {
	From          string `query:"from" doc:"RFC3339 timestamp. Only transactions created at or after this time."`
	To            string `query:"to" doc:"RFC3339 timestamp. Only transactions created strictly before this time."`
	EffectiveFrom string `query:"effective_from" doc:"RFC3339 timestamp. Only transactions whose value date (effective_at, falling back to created_at when unset) is at or after this time."`
	EffectiveTo   string `query:"effective_to" doc:"RFC3339 timestamp. Only transactions whose value date (effective_at, falling back to created_at when unset) is strictly before this time."`
	Reference     string `query:"reference" doc:"Exact match on the transaction's reference."`
	Format        string `query:"format" default:"csv" enum:"csv,json" doc:"csv (default): one row per posting, with a header row. json: an array of the same transaction bodies GET /v1/transactions returns."`
}

// TransactionExportOutput is a raw csv or json export body (Task 4.4, audit
// A7.2). Body is a []byte, which huma writes out verbatim instead of
// JSON-encoding it (see huma's ContentTypeFilter / raw-body handling), so
// ContentType and, for csv, ContentDisposition are set explicitly here
// rather than inferred from a struct tag.
type TransactionExportOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition" doc:"Set to attachment for csv; unset for json"`
	Truncated          bool   `header:"Export-Truncated" doc:"true if the tenant's matching history exceeds the export row cap and this export contains only the newest rows up to it"`
	Body               []byte
}

// parseTransactionFilter builds a domain.TransactionFilter from the list and
// export endpoints' shared from/to/effective_from/effective_to/reference
// query params (Task 4.4, audit A7.2; effective_from/effective_to added by
// follow-up F2, audit A1.3 partial). Each of the four timestamps is
// optional RFC3339; an empty string leaves that side of the filter unset.
// reference is an exact-match filter, also optional: an empty string leaves
// it unset too, since Transaction.Validate already rejects an empty,
// non-nil reference, so "" can never be a real reference to match against.
func parseTransactionFilter(from, to, effectiveFrom, effectiveTo, reference string) (domain.TransactionFilter, error) {
	var filter domain.TransactionFilter
	if from != "" {
		t, err := time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return filter, huma.Error422UnprocessableEntity("from must be an RFC3339 timestamp")
		}
		filter.From = &t
	}
	if to != "" {
		t, err := time.Parse(time.RFC3339Nano, to)
		if err != nil {
			return filter, huma.Error422UnprocessableEntity("to must be an RFC3339 timestamp")
		}
		filter.To = &t
	}
	if effectiveFrom != "" {
		t, err := time.Parse(time.RFC3339Nano, effectiveFrom)
		if err != nil {
			return filter, huma.Error422UnprocessableEntity("effective_from must be an RFC3339 timestamp")
		}
		filter.EffectiveFrom = &t
	}
	if effectiveTo != "" {
		t, err := time.Parse(time.RFC3339Nano, effectiveTo)
		if err != nil {
			return filter, huma.Error422UnprocessableEntity("effective_to must be an RFC3339 timestamp")
		}
		filter.EffectiveTo = &t
	}
	if reference != "" {
		filter.Reference = &reference
	}
	return filter, nil
}

// transactionsCSV renders items as a flattened, posting-level CSV (Task 4.4,
// audit A7.2): one row per posting, not per transaction, since a transaction
// spans a variable number of postings and a csv needs one fixed row shape
// throughout. The header matches the brief exactly: transaction_id,
// posting_id, account_id, amount, currency, description, reference,
// created_at, effective_at.
func transactionsCSV(items []domain.TransactionListItem) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{
		"transaction_id", "posting_id", "account_id", "amount", "currency",
		"description", "reference", "created_at", "effective_at",
	})
	for _, item := range items {
		t := item.Transaction
		reference := ""
		if t.Reference != nil {
			reference = *t.Reference
		}
		effectiveAt := ""
		if t.EffectiveAt != nil {
			effectiveAt = t.EffectiveAt.UTC().Format(time.RFC3339Nano)
		}
		createdAt := item.CreatedAt.UTC().Format(time.RFC3339Nano)
		for _, p := range t.Postings {
			_ = w.Write([]string{
				t.ID,
				p.ID,
				p.AccountID,
				strconv.FormatInt(p.Amount.Amount(), 10),
				string(p.Amount.Currency()),
				csvSafeField(p.Description),
				csvSafeField(reference),
				createdAt,
				effectiveAt,
			})
		}
	}
	w.Flush()
	return buf.Bytes()
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
	body := TransactionBody{ID: t.ID, Postings: postings, ReversesTransactionID: t.ReversesTransactionID, Reference: t.Reference}
	// EffectiveAt is always populated by the time a transaction reaches here
	// (CreateTransaction resolves the read-time fallback to created_at
	// itself, see postgres.txRepo.CreateTransaction and
	// Repository.transactionFromRow), but toTransactionBody stays defensive
	// against a nil for any future caller that builds a domain.Transaction
	// by hand.
	if t.EffectiveAt != nil {
		body.EffectiveAt = *t.EffectiveAt
	}
	return body
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
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"255" doc:"Required. Retrying with the same key returns the original conversion; reusing a key with a different body returns 409. Keys are remembered for a bounded replay window (server-configured IDEMPOTENCY_TTL, default 24h): once a key expires it is treated as unused, so reusing it after the window starts a brand-new conversion instead of replaying or conflicting."`
	Body           struct {
		FromAccount  string `json:"from_account" format:"uuid" doc:"Account to debit, in its own currency"`
		ToAccount    string `json:"to_account" format:"uuid" doc:"Account to credit; its currency is the conversion's destination currency"`
		SourceAmount int64  `json:"source_amount" doc:"Amount to convert, in minor units of the from account's currency; must be positive"`
	}
}

// ConvertResponseBody is the JSON shape of a successful convert: the posted
// transaction (source account, source clearing, destination clearing,
// destination account) plus the FX rate detail actually applied. Pending is
// set instead (ADR-025) when the conversion exceeded the approval threshold
// and was held rather than posted; Transaction and FX are left zero in that
// case.
type ConvertResponseBody struct {
	Transaction TransactionBody `json:"transaction"`
	FX          FXDetailBody    `json:"fx"`
	Pending     *PendingBody    `json:"pending,omitempty" doc:"Set instead of transaction/fx when the conversion exceeded the approval threshold (ADR-025) and the request was held for approval rather than posted."`
}

// ConvertOutput is the convert response. Replayed mirrors
// CreateTransactionOutput: true when an Idempotency-Key matched an earlier
// convert request and the original conversion was returned unchanged. Status
// is 201 (converted) or 202 (ADR-025: held for approval); see
// CreateTransactionOutput's Status doc comment for the mechanism.
type ConvertOutput struct {
	Status   int
	Replayed bool `header:"Idempotent-Replayed"`
	Body     ConvertResponseBody
}

func registerTransactions(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "create-transaction",
		Method:        http.MethodPost,
		Path:          "/v1/transactions",
		Summary:       "Post a transaction",
		Description:   "Posts a balanced transaction whose postings sum to zero in the given currency. Requires an Idempotency-Key header to make retries safe: a repeat with the same key returns the original transaction (with Idempotent-Replayed: true) instead of posting again, and reusing a key with a different body returns 409 Conflict. A key's replay window is bounded (server-configured IDEMPOTENCY_TTL, default 24h): after that window a reused key is treated as never having been used, so the same header value posts a new transaction instead of replaying or conflicting; retry within the window if you need the original guarantee. An optional reference (an external id for reconciliation) must be unique per tenant when supplied: reusing one already in use returns 409 Conflict too, distinct from the idempotency-key conflict. An optional effective_at sets the value date; omitted, it defaults to the post time.",
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
		txn := &domain.Transaction{Postings: postings, Reference: in.Body.Reference, EffectiveAt: in.Body.EffectiveAt}
		idem := &domain.Idempotency{Key: in.IdempotencyKey}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		ctx = ledger.WithActor(ctx, actorFromCtx(ctx, tenant))
		replayed, err := deps.Transactions.Post(ctx, tenant, txn, idem)
		if err != nil {
			// ADR-025: an over-threshold post is held as a pending instead of
			// posted. holdForApproval already committed the pending row and its
			// approval.requested lifecycle event by the time this error
			// reaches here, so there is nothing left to write, only to report:
			// 202 Accepted with the pending resource instead of the usual 201.
			if pending, held := ledger.AsHeldForApproval(err); held {
				out := &CreateTransactionOutput{Status: http.StatusAccepted}
				body := toPendingBody(pending)
				out.Body.Pending = &body
				return out, nil
			}
			return nil, toHumaErr(err)
		}
		out := &CreateTransactionOutput{Status: http.StatusCreated, Replayed: replayed}
		out.Body.TransactionBody = toTransactionBody(*txn)
		return out, nil
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
		Description:   "Converts source_amount from the from_account's currency into the to_account's currency, at the tenant's current FX rate, and posts the four resulting legs (debit the from account, credit its currency's clearing account, debit the to currency's clearing account, credit the to account) atomically. The destination currency always comes from the to account: a client cannot supply a rate or target currency directly. Requires an Idempotency-Key header, the same as POST /v1/transactions: a repeat with the same key returns the original conversion (with Idempotent-Replayed: true) instead of converting again, and reusing a key with a different body returns 409 Conflict. The same bounded replay window applies (server-configured IDEMPOTENCY_TTL, default 24h): past it, a reused key is treated as never having been used and converts again instead of replaying or conflicting.",
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
		ctx = ledger.WithActor(ctx, actorFromCtx(ctx, tenant))
		txn, replayed, err := deps.Transactions.Convert(ctx, tenant, req, idem)
		if err != nil {
			// ADR-025: see create-transaction's identical check above.
			if pending, held := ledger.AsHeldForApproval(err); held {
				body := toPendingBody(pending)
				return &ConvertOutput{Status: http.StatusAccepted, Body: ConvertResponseBody{Pending: &body}}, nil
			}
			return nil, toHumaErr(err)
		}
		return &ConvertOutput{
			Status:   http.StatusCreated,
			Replayed: replayed,
			Body: ConvertResponseBody{
				Transaction: toTransactionBody(*txn),
				FX:          toFXDetailBody(txn.FX),
			},
		}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "reverse-transaction",
		Method:        http.MethodPost,
		Path:          "/v1/transactions/{id}/reverse",
		Summary:       "Reverse a transaction",
		Description:   "Posts the negated legs of the transaction named by id as a new, linked transaction (postings are append-only, ADR-001: this never mutates the original). Idempotent: a transaction can be reversed at most once, so calling this again for the same id returns the SAME reversal, with Already-Reversed: true, instead of posting a second one. Reversing a transaction that is itself a reversal returns 422.",
		Tags:          []string{"transactions"},
		DefaultStatus: http.StatusCreated,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *transactionIDInput) (*ReverseTransactionOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		ctx = ledger.WithActor(ctx, actorFromCtx(ctx, tenant))
		reversal, alreadyReversed, err := deps.Transactions.ReverseTransaction(ctx, tenant, in.ID)
		if err != nil {
			// ADR-025: see create-transaction's identical check above.
			if pending, held := ledger.AsHeldForApproval(err); held {
				out := &ReverseTransactionOutput{Status: http.StatusAccepted}
				body := toPendingBody(pending)
				out.Body.Pending = &body
				return out, nil
			}
			return nil, toHumaErr(err)
		}
		out := &ReverseTransactionOutput{Status: http.StatusCreated, AlreadyReversed: alreadyReversed}
		out.Body.TransactionBody = toTransactionBody(*reversal)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-transactions",
		Method:      http.MethodGet,
		Path:        "/v1/transactions",
		Summary:     "List transactions",
		Description: "Lists the tenant's transactions, newest first, optionally filtered by a created_at date range (from inclusive, to exclusive), a value-date range (effective_from inclusive, effective_to exclusive, matched against effective_at, falling back to created_at when unset), and/or an exact reference match. Keyset paged: pass the response's next_cursor back as cursor to fetch the next page; next_cursor is null on the last page.",
		Tags:        []string{"transactions"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *TransactionListInput) (*TransactionListOutput, error) {
		filter, err := parseTransactionFilter(in.From, in.To, in.EffectiveFrom, in.EffectiveTo, in.Reference)
		if err != nil {
			return nil, err
		}
		after, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		// Requested as limit+1, not limit: getting more rows back than the
		// caller asked for is how hasMore below detects a next page exists
		// without a second round trip (paging.Page's doc comment).
		rows, err := deps.Transactions.ListTransactions(ctx, tenant, filter, after, in.Limit+1)
		if err != nil {
			return nil, toHumaErr(err)
		}
		page, hasMore := paging.Page(rows, in.Limit)

		out := &TransactionListOutput{}
		out.Body.Transactions = make([]TransactionBody, 0, len(page))
		for _, item := range page {
			out.Body.Transactions = append(out.Body.Transactions, toTransactionBody(item.Transaction))
		}
		if hasMore {
			last := page[len(page)-1]
			c := encodeCursor(last.CreatedAt, last.Transaction.ID)
			out.Body.NextCursor = &c
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "export-transactions",
		Method:      http.MethodGet,
		Path:        "/v1/transactions/export",
		Summary:     "Export transactions",
		Description: "Exports the tenant's transactions matching the same from/to/effective_from/effective_to/reference filters as GET /v1/transactions, as csv (default, one row per posting) or json (an array of transaction bodies). Not paged: bounded instead at a fixed row cap, so a tenant with a longer matching history than the cap gets only the newest rows up to it, reported via the Export-Truncated response header. gRPC has no equivalent RPC: a streaming CSV export does not fit its single-response model, so export stays REST-only.",
		Tags:        []string{"transactions"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *TransactionExportInput) (*TransactionExportOutput, error) {
		filter, err := parseTransactionFilter(in.From, in.To, in.EffectiveFrom, in.EffectiveTo, in.Reference)
		if err != nil {
			return nil, err
		}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		items, truncated, err := deps.Transactions.ExportTransactions(ctx, tenant, filter)
		if err != nil {
			return nil, toHumaErr(err)
		}

		out := &TransactionExportOutput{Truncated: truncated}
		if in.Format == "json" {
			bodies := make([]TransactionBody, 0, len(items))
			for _, item := range items {
				bodies = append(bodies, toTransactionBody(item.Transaction))
			}
			b, err := json.Marshal(bodies)
			if err != nil {
				return nil, huma.Error500InternalServerError("marshal export")
			}
			out.ContentType = "application/json"
			out.Body = b
			return out, nil
		}
		out.ContentType = "text/csv"
		out.ContentDisposition = `attachment; filename="transactions.csv"`
		out.Body = transactionsCSV(items)
		return out, nil
	})
}
