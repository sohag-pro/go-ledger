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
)

// AccountBody is the JSON shape of an account in responses.
type AccountBody struct {
	ID       string `json:"id" doc:"Account id (UUID)"`
	Name     string `json:"name"`
	Type     string `json:"type" doc:"One of: asset, liability, equity, income, expense"`
	Currency string `json:"currency" doc:"ISO 4217 code, e.g. USD"`
	// Status is the account's lifecycle gate (Task 5.5, audit A1.5): active,
	// frozen, or closed. See POST /v1/accounts/{id}/status to change it.
	Status string `json:"status" doc:"One of: active, frozen, closed"`
	// MinBalance is the optional floor enforced on this account's derived
	// balance, in minor units (Task 5.5, audit A1.5). null means no floor.
	MinBalance *int64 `json:"min_balance,omitempty" doc:"Optional minimum balance floor, in minor units. Omitted when unset."`
	// PartyReference and PartyType are optional linkage metadata for an
	// external KYC/party system (Task 6.1, audit A9.1). Both are omitted
	// when unset; this package does not validate them beyond a length cap,
	// the actual party/KYC system is external.
	PartyReference *string `json:"party_reference,omitempty" doc:"Optional external customer/party id, for linking to an external KYC system. Omitted when unset."`
	PartyType      *string `json:"party_type,omitempty" doc:"Optional free-text party classification, e.g. individual or business. Omitted when unset."`
}

func toAccountBody(a domain.Account) AccountBody {
	return AccountBody{
		ID:             a.ID,
		Name:           a.Name,
		Type:           a.Type.String(),
		Currency:       string(a.Currency),
		Status:         string(a.Status),
		MinBalance:     a.MinBalance,
		PartyReference: a.PartyReference,
		PartyType:      a.PartyType,
	}
}

// CreateAccountInput is the create-account request body. Currency is
// optional (omitempty): when the caller omits it, the server stamps the
// deployment's configured DEFAULT_CURRENCY instead (ADR-014). MinBalance is
// also optional (Task 5.5, audit A1.5): every account is created active, so
// there is no status field here (see POST /v1/accounts/{id}/status).
type CreateAccountInput struct {
	Body struct {
		Name       string `json:"name" minLength:"1" maxLength:"200" doc:"Human-readable account name"`
		Type       string `json:"type" enum:"asset,liability,equity,income,expense" doc:"Fundamental account class"`
		Currency   string `json:"currency,omitempty" pattern:"^[A-Z]{3}$" doc:"ISO 4217 alphabetic code. Defaults to the server's DEFAULT_CURRENCY when omitted."`
		MinBalance *int64 `json:"min_balance,omitempty" doc:"Optional minimum balance floor, in minor units. Omit for no floor."`
		// PartyReference and PartyType (Task 6.1, audit A9.1) are optional
		// linkage metadata for an external KYC/party system: an id and a
		// free-text classification (e.g. "individual", "business"). Neither
		// is validated beyond a length cap; the party/KYC system itself is
		// external and out of scope for this service.
		PartyReference *string `json:"party_reference,omitempty" maxLength:"256" doc:"Optional external customer/party id (KYC linkage). Omit if not linked to an external party system."`
		PartyType      *string `json:"party_type,omitempty" maxLength:"256" doc:"Optional free-text party classification, e.g. individual or business."`
	}
}

// SetAccountStatusInput is the set-account-status request: a path id plus
// the new status (Task 5.5, audit A1.5).
type SetAccountStatusInput struct {
	ID   string `path:"id" format:"uuid" doc:"Account id"`
	Body struct {
		Status string `json:"status" enum:"active,frozen,closed" doc:"New account status"`
	}
}

// AccountOutput wraps an account in a response.
type AccountOutput struct {
	Body AccountBody
}

type accountIDInput struct {
	ID string `path:"id" format:"uuid" doc:"Account id"`
}

// ListAccountsInput is the list-accounts request: a capped limit, no cursor.
type ListAccountsInput struct {
	Limit int `query:"limit" default:"100" minimum:"1" maximum:"500" doc:"Max accounts to return"`
}

// AccountsOutput is the list-accounts response.
type AccountsOutput struct {
	Body struct {
		Accounts []AccountBody `json:"accounts"`
	}
}

// BalanceOutput is the account balance response.
type BalanceOutput struct {
	Body struct {
		AccountID string `json:"account_id"`
		Amount    int64  `json:"amount" doc:"Signed balance in minor units (e.g. cents)"`
		Currency  string `json:"currency"`
	}
}

// StatementInput is the account statement request: a path id plus keyset paging.
type StatementInput struct {
	ID     string `path:"id" format:"uuid" doc:"Account id"`
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"200" doc:"Max entries per page"`
	Cursor string `query:"cursor" doc:"Opaque cursor from a previous page's next_cursor"`
}

// StatementEntryBody is one line of a statement: a posting affecting the account,
// with the running balance as of that posting.
type StatementEntryBody struct {
	ID             string    `json:"id" doc:"Posting id"`
	TransactionID  string    `json:"transaction_id"`
	Amount         int64     `json:"amount" doc:"Signed posting amount in minor units"`
	RunningBalance int64     `json:"running_balance" doc:"Account balance as of this posting, in minor units"`
	Description    string    `json:"description"`
	CreatedAt      time.Time `json:"created_at"`
}

// StatementOutput is one page of an account statement.
type StatementOutput struct {
	Body struct {
		AccountID  string               `json:"account_id"`
		Currency   string               `json:"currency"`
		Entries    []StatementEntryBody `json:"entries"`
		NextCursor *string              `json:"next_cursor" doc:"Cursor for the next page, or null if this is the last page"`
	}
}

// StatementExportInput is the export-account-statement request: a path id,
// an optional from/to created_at window (RFC3339, half-open like
// TransactionExportInput's), plus format (Task 6.3, audit A9.2). Unlike GET
// /v1/accounts/{id}/statement this is not keyset paged: it returns every
// matching entry up to ledger.MaxExportRows in a single response, the same
// bounded-export shape GET /v1/transactions/export uses.
type StatementExportInput struct {
	ID     string `path:"id" format:"uuid" doc:"Account id"`
	From   string `query:"from" doc:"RFC3339 timestamp. Only postings created at or after this time."`
	To     string `query:"to" doc:"RFC3339 timestamp. Only postings created strictly before this time."`
	Format string `query:"format" default:"csv" enum:"csv,json" doc:"csv (default): one row per posting, with a header row. json: an array of statement entries."`
}

// StatementExportOutput is a raw csv or json export body, the same shape
// TransactionExportOutput uses (Task 6.3, audit A9.2): Body is a []byte,
// which huma writes out verbatim instead of JSON-encoding it.
type StatementExportOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition" doc:"Set to attachment for csv; unset for json"`
	Truncated          bool   `header:"Export-Truncated" doc:"true if the account's matching posting history exceeds the export row cap and this export contains only the newest rows up to it"`
	Body               []byte
}

// statementExportJSONEntry is the json format's per-entry shape: the same
// fields StatementEntryBody carries, plus TransactionID (already present)
// with the csv header's exact column names as json keys, so the two formats
// describe the identical data.
type statementExportJSONEntry struct {
	PostingID      string    `json:"posting_id"`
	TransactionID  string    `json:"transaction_id"`
	CreatedAt      time.Time `json:"created_at"`
	Amount         int64     `json:"amount"`
	Currency       string    `json:"currency"`
	RunningBalance int64     `json:"running_balance"`
	Description    string    `json:"description"`
}

// statementCSV renders entries as a csv with the header the brief specifies
// exactly: posting_id, transaction_id, created_at, amount, currency,
// running_balance, description (Task 6.3, audit A9.2).
func statementCSV(entries []domain.StatementEntry, currency string) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{
		"posting_id", "transaction_id", "created_at", "amount", "currency",
		"running_balance", "description",
	})
	for _, e := range entries {
		_ = w.Write([]string{
			e.ID,
			e.TransactionID,
			e.CreatedAt.UTC().Format(time.RFC3339Nano),
			strconv.FormatInt(e.Amount.Amount(), 10),
			currency,
			strconv.FormatInt(e.RunningBalance.Amount(), 10),
			e.Description,
		})
	}
	w.Flush()
	return buf.Bytes()
}

func registerAccounts(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "create-account",
		Method:        http.MethodPost,
		Path:          "/v1/accounts",
		Summary:       "Create an account",
		Tags:          []string{"accounts"},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxRequestBodyBytes,
		Security:      bearerSecurity,
	}, func(ctx context.Context, in *CreateAccountInput) (*AccountOutput, error) {
		at, err := domain.ParseAccountType(in.Body.Type)
		if err != nil {
			return nil, toHumaErr(err)
		}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		acct := &domain.Account{
			Name:           in.Body.Name,
			Type:           at,
			Currency:       domain.Currency(in.Body.Currency),
			MinBalance:     in.Body.MinBalance,
			PartyReference: in.Body.PartyReference,
			PartyType:      in.Body.PartyType,
		}
		if err := deps.Accounts.Create(ctx, tenant, acct); err != nil {
			return nil, toHumaErr(err)
		}
		return &AccountOutput{Body: toAccountBody(*acct)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-accounts",
		Method:      http.MethodGet,
		Path:        "/v1/accounts",
		Summary:     "List accounts",
		Tags:        []string{"accounts"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *ListAccountsInput) (*AccountsOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		accts, err := deps.Accounts.List(ctx, tenant, in.Limit)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &AccountsOutput{}
		out.Body.Accounts = make([]AccountBody, 0, len(accts))
		for _, a := range accts {
			out.Body.Accounts = append(out.Body.Accounts, toAccountBody(a))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-account",
		Method:      http.MethodGet,
		Path:        "/v1/accounts/{id}",
		Summary:     "Get an account",
		Tags:        []string{"accounts"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *accountIDInput) (*AccountOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		acct, err := deps.Accounts.Get(ctx, tenant, in.ID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &AccountOutput{Body: toAccountBody(acct)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:  "set-account-status",
		Method:       http.MethodPost,
		Path:         "/v1/accounts/{id}/status",
		Summary:      "Freeze, close, or reactivate an account",
		Tags:         []string{"accounts"},
		MaxBodyBytes: MaxRequestBodyBytes,
		Security:     bearerSecurity,
	}, func(ctx context.Context, in *SetAccountStatusInput) (*AccountOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		acct, err := deps.Accounts.SetStatus(ctx, tenant, in.ID, domain.AccountStatus(in.Body.Status))
		if err != nil {
			return nil, toHumaErr(err)
		}
		return &AccountOutput{Body: toAccountBody(acct)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-account-balance",
		Method:      http.MethodGet,
		Path:        "/v1/accounts/{id}/balance",
		Summary:     "Get an account's balance",
		Tags:        []string{"accounts"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *accountIDInput) (*BalanceOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		bal, err := deps.Accounts.Balance(ctx, tenant, in.ID)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &BalanceOutput{}
		out.Body.AccountID = in.ID
		out.Body.Amount = bal.Amount()
		out.Body.Currency = string(bal.Currency())
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-account-statement",
		Method:      http.MethodGet,
		Path:        "/v1/accounts/{id}/statement",
		Summary:     "List an account's postings with running balance",
		Tags:        []string{"accounts"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *StatementInput) (*StatementOutput, error) {
		after, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		acct, entries, err := deps.Accounts.Statement(ctx, tenant, in.ID, after, in.Limit)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &StatementOutput{}
		out.Body.AccountID = acct.ID
		out.Body.Currency = string(acct.Currency)
		out.Body.Entries = make([]StatementEntryBody, 0, len(entries))
		for _, e := range entries {
			out.Body.Entries = append(out.Body.Entries, StatementEntryBody{
				ID:             e.ID,
				TransactionID:  e.TransactionID,
				Amount:         e.Amount.Amount(),
				RunningBalance: e.RunningBalance.Amount(),
				Description:    e.Description,
				CreatedAt:      e.CreatedAt,
			})
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
		OperationID: "export-account-statement",
		Method:      http.MethodGet,
		Path:        "/v1/accounts/{id}/statement/export",
		Summary:     "Export an account's period statement",
		Description: "Exports the account's postings within an optional from/to created_at window (from inclusive, to exclusive), each with its running balance, as csv (default, one row per posting) or json. Not paged: bounded instead at a fixed row cap (the same ledger.MaxExportRows GET /v1/transactions/export uses), so a longer matching history than the cap yields only the newest rows up to it, reported via the Export-Truncated response header.",
		Tags:        []string{"accounts"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, in *StatementExportInput) (*StatementExportOutput, error) {
		from, to, err := parseTimeRange(in.From, in.To)
		if err != nil {
			return nil, err
		}
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		acct, entries, truncated, err := deps.Accounts.StatementExport(ctx, tenant, in.ID, from, to)
		if err != nil {
			return nil, toHumaErr(err)
		}

		out := &StatementExportOutput{Truncated: truncated}
		if in.Format == "json" {
			items := make([]statementExportJSONEntry, 0, len(entries))
			for _, e := range entries {
				items = append(items, statementExportJSONEntry{
					PostingID:      e.ID,
					TransactionID:  e.TransactionID,
					CreatedAt:      e.CreatedAt,
					Amount:         e.Amount.Amount(),
					Currency:       string(acct.Currency),
					RunningBalance: e.RunningBalance.Amount(),
					Description:    e.Description,
				})
			}
			b, err := json.Marshal(items)
			if err != nil {
				return nil, huma.Error500InternalServerError("marshal export")
			}
			out.ContentType = "application/json"
			out.Body = b
			return out, nil
		}
		out.ContentType = "text/csv"
		out.ContentDisposition = `attachment; filename="statement.csv"`
		out.Body = statementCSV(entries, string(acct.Currency))
		return out, nil
	})
}

// parseTimeRange parses the optional from/to RFC3339 query parameters shared
// by StatementExportInput, mirroring parseTransactionFilter's from/to
// handling (Task 4.4, audit A7.2) without the reference field a per-account
// statement export has no use for. An empty string leaves that side of the
// window unset (nil).
func parseTimeRange(from, to string) (*time.Time, *time.Time, error) {
	var f, t *time.Time
	if from != "" {
		parsed, err := time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return nil, nil, huma.Error422UnprocessableEntity("from must be an RFC3339 timestamp")
		}
		f = &parsed
	}
	if to != "" {
		parsed, err := time.Parse(time.RFC3339Nano, to)
		if err != nil {
			return nil, nil, huma.Error422UnprocessableEntity("to must be an RFC3339 timestamp")
		}
		t = &parsed
	}
	return f, t, nil
}
