package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

const testTenant = "00000000-0000-0000-0000-000000000001"

// testAPIKeyPlaintext is the bearer token every handler test authenticates
// with by default (do, postJSON, getJSON all set it). newAPIRouter provisions
// it against testTenant on the fake repo it is given, so every existing test
// keeps exercising the real auth middleware instead of bypassing it.
const testAPIKeyPlaintext = "glk_handlers-test-default-key" //nolint:gosec // test fixture key, not a real credential

// fakeRepo is an in-memory domain.Repository for handler tests: no database, no
// concurrency semantics, just enough to exercise the HTTP layer end to end.
type fakeRepo struct {
	accounts map[string]domain.Account
	txns     map[string]domain.Transaction
	postings []postingRec
	clock    int64
	idem     map[string]domain.IdempotencyRecord // key -> record
	audit    []domain.AuditEntry
	apiKeys  map[string]domain.APIKey // key_hash -> resolved key
}

type postingRec struct {
	id, txnID, accountID, description string
	amount                            int64
	createdAt                         time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		accounts: map[string]domain.Account{},
		txns:     map[string]domain.Transaction{},
		idem:     map[string]domain.IdempotencyRecord{},
		apiKeys:  map[string]domain.APIKey{},
	}
}

func (f *fakeRepo) CreateAccount(_ context.Context, _ string, a *domain.Account) error {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if err := a.Validate(); err != nil {
		return err
	}
	f.accounts[a.ID] = *a
	return nil
}

func (f *fakeRepo) GetAccount(_ context.Context, _, id string) (domain.Account, error) {
	a, ok := f.accounts[id]
	if !ok {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	return a, nil
}

func (f *fakeRepo) ListAccounts(_ context.Context, _ string, limit int) ([]domain.Account, error) {
	out := make([]domain.Account, 0, len(f.accounts))
	for _, a := range f.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRepo) CreateTransaction(_ context.Context, _ string, t *domain.Transaction) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	f.txns[t.ID] = *t
	for _, p := range t.Postings {
		f.clock++
		f.postings = append(f.postings, postingRec{
			id:          uuid.NewString(),
			txnID:       t.ID,
			accountID:   p.AccountID,
			description: p.Description,
			amount:      p.Amount.Amount(),
			createdAt:   time.Unix(f.clock, 0).UTC(),
		})
	}
	return nil
}

func (f *fakeRepo) GetTransaction(_ context.Context, _, id string) (domain.Transaction, error) {
	t, ok := f.txns[id]
	if !ok {
		return domain.Transaction{}, domain.ErrTransactionNotFound
	}
	return t, nil
}

func (f *fakeRepo) InsertIdempotencyKey(_ context.Context, _, key, fingerprint, transactionID string) error {
	if _, ok := f.idem[key]; ok {
		return domain.ErrDuplicateIdempotencyKey
	}
	f.idem[key] = domain.IdempotencyRecord{Key: key, Fingerprint: fingerprint, TransactionID: transactionID}
	return nil
}

func (f *fakeRepo) AppendAudit(_ context.Context, tenantID string, e domain.AuditEntry) error {
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	f.clock++
	e.CreatedAt = time.Unix(f.clock, 0).UTC()
	// Mirror the real repository's chain extension: prev is the last row's
	// RowHash (genesis if this is the first row appended by this fake repo).
	prev := domain.AuditGenesisHash
	if len(f.audit) > 0 {
		prev = f.audit[len(f.audit)-1].RowHash
	}
	e.PrevHash = prev
	e.RowHash = domain.ComputeAuditRowHash(tenantID, e, prev)
	f.audit = append(f.audit, e)
	return nil
}

// ListAuditForVerify returns every audit row this fake repo holds, oldest
// first, mirroring the postgres adapter's ordering. It does not scope by
// tenant: fakeRepo is single-tenant in these handler tests.
func (f *fakeRepo) ListAuditForVerify(_ context.Context, _ string) ([]domain.AuditEntry, error) {
	out := make([]domain.AuditEntry, len(f.audit))
	copy(out, f.audit)
	return out, nil
}

func (f *fakeRepo) GetIdempotencyKey(_ context.Context, _, key string) (domain.IdempotencyRecord, error) {
	rec, ok := f.idem[key]
	if !ok {
		return domain.IdempotencyRecord{}, domain.ErrIdempotencyKeyNotFound
	}
	return rec, nil
}

func (f *fakeRepo) ListAuditByTransaction(_ context.Context, _, transactionID string) ([]domain.AuditEntry, error) {
	out := make([]domain.AuditEntry, 0)
	for _, e := range f.audit {
		if e.TransactionID == transactionID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeRepo) ListAuditByAccount(_ context.Context, _, accountID string, after *domain.StatementCursor, limit int) ([]domain.AuditEntry, error) {
	txns := map[string]bool{}
	for _, p := range f.postings {
		if p.accountID == accountID {
			txns[p.txnID] = true
		}
	}
	matched := make([]domain.AuditEntry, 0)
	for _, e := range f.audit {
		if txns[e.TransactionID] {
			matched = append(matched, e)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.Before(matched[j].CreatedAt)
		}
		return matched[i].ID < matched[j].ID
	})
	// newest first
	out := make([]domain.AuditEntry, 0, len(matched))
	for i := len(matched) - 1; i >= 0; i-- {
		e := matched[i]
		if after != nil {
			if e.CreatedAt.After(after.CreatedAt) || (e.CreatedAt.Equal(after.CreatedAt) && e.ID >= after.ID) {
				continue
			}
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRepo) Balance(_ context.Context, _, accountID string) (domain.Money, error) {
	a, ok := f.accounts[accountID]
	if !ok {
		return domain.Money{}, domain.ErrAccountNotFound
	}
	var sum int64
	for _, p := range f.postings {
		if p.accountID == accountID {
			sum += p.amount
		}
	}
	return domain.NewMoney(sum, a.Currency)
}

func (f *fakeRepo) Statement(_ context.Context, _, accountID string, currency domain.Currency, after *domain.StatementCursor, limit int) ([]domain.StatementEntry, error) {
	recs := make([]postingRec, 0)
	for _, p := range f.postings {
		if p.accountID == accountID {
			recs = append(recs, p)
		}
	}
	sort.Slice(recs, func(i, j int) bool {
		if !recs[i].createdAt.Equal(recs[j].createdAt) {
			return recs[i].createdAt.Before(recs[j].createdAt)
		}
		return recs[i].id < recs[j].id
	})
	var run int64
	asc := make([]domain.StatementEntry, 0, len(recs))
	for _, r := range recs {
		run += r.amount
		amt, _ := domain.NewMoney(r.amount, currency)
		rb, _ := domain.NewMoney(run, currency)
		asc = append(asc, domain.StatementEntry{
			ID: r.id, TransactionID: r.txnID, Amount: amt, RunningBalance: rb,
			Description: r.description, CreatedAt: r.createdAt,
		})
	}
	// newest first
	out := make([]domain.StatementEntry, 0, len(asc))
	for i := len(asc) - 1; i >= 0; i-- {
		e := asc[i]
		if after != nil {
			if e.CreatedAt.After(after.CreatedAt) || (e.CreatedAt.Equal(after.CreatedAt) && e.ID >= after.ID) {
				continue
			}
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRepo) RunInTx(ctx context.Context, fn func(context.Context, domain.Tx) error) error {
	return fn(ctx, f)
}

func (f *fakeRepo) GetAPIKeyByHash(_ context.Context, hash string) (domain.APIKey, error) {
	k, ok := f.apiKeys[hash]
	if !ok {
		return domain.APIKey{}, domain.ErrAPIKeyNotFound
	}
	return k, nil
}

func (f *fakeRepo) InsertAPIKey(_ context.Context, k domain.APIKey, keyHash string) error {
	if k.ID == "" {
		k.ID = uuid.NewString()
	}
	f.apiKeys[keyHash] = k
	return nil
}

var _ domain.Repository = (*fakeRepo)(nil)

// newAPIRouter wires the API over repo, provisioning testAPIKeyPlaintext
// against testTenant so the default request helpers below (do, postJSON,
// getJSON) authenticate as testTenant through the real auth middleware rather
// than bypassing it.
func newAPIRouter(repo domain.Repository) chi.Router {
	if err := repo.InsertAPIKey(context.Background(),
		domain.APIKey{TenantID: testTenant, Name: "handlers test default key"},
		domain.HashAPIKey(testAPIKeyPlaintext),
	); err != nil {
		panic("newAPIRouter: provision default test key: " + err.Error())
	}

	r := chi.NewRouter()
	New(r, Deps{
		Accounts:     ledger.NewAccountService(repo),
		Transactions: ledger.NewTransactionService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), nil),
		Audit:        ledger.NewAuditService(repo),
		Auth:         auth.NewResolver(repo, time.Minute),
	})
	return r
}

// do issues a request authenticated as testTenant. An optional trailing
// headers map (e.g. {"Idempotency-Key": "..."}) is applied on top of the
// defaults; only the first map, if any, is used.
func do(t *testing.T, r chi.Router, method, path string, body any, headers ...map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKeyPlaintext)
	if len(headers) > 0 {
		for k, v := range headers[0] {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// postJSON POSTs a raw JSON body with optional extra headers, e.g. Idempotency-Key.
//
//nolint:unparam // path is a general test-helper parameter; only one literal is in use today.
func postJSON(t *testing.T, r chi.Router, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKeyPlaintext)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// getJSON GETs path with no body, authenticated as testTenant.
func getJSON(t *testing.T, r chi.Router, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKeyPlaintext)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func createAccount(t *testing.T, r chi.Router, name, typ string) string {
	t.Helper()
	rec := do(t, r, http.MethodPost, "/v1/accounts", map[string]string{"name": name, "type": typ, "currency": "USD"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create account %s: status %d (%s)", name, rec.Code, rec.Body.String())
	}
	var out AccountBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode account: %v", err)
	}
	return out.ID
}

func TestCreateAccount(t *testing.T) {
	r := newAPIRouter(newFakeRepo())

	t.Run("happy path 201", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/accounts",
			map[string]string{"name": "Cash", "type": "asset", "currency": "USD"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
		var out AccountBody
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.ID == "" || out.Type != "asset" || out.Currency != "USD" {
			t.Errorf("unexpected body: %+v", out)
		}
	})

	t.Run("bad type 422", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/accounts",
			map[string]string{"name": "X", "type": "bogus", "currency": "USD"})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422", rec.Code)
		}
	})

	t.Run("bad currency 422", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/accounts",
			map[string]string{"name": "X", "type": "asset", "currency": "usd"})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422", rec.Code)
		}
	})
}

func TestGetAccountNotFound(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	rec := do(t, r, http.MethodGet, "/v1/accounts/"+uuid.NewString(), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
}

func TestListAccounts(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	createAccount(t, r, "Cash", "asset")
	createAccount(t, r, "Revenue", "income")

	rec := do(t, r, http.MethodGet, "/v1/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Accounts []AccountBody `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(out.Accounts))
	}
	// Ordered by name: Cash before Revenue.
	if out.Accounts[0].Name != "Cash" || out.Accounts[1].Name != "Revenue" {
		t.Errorf("unexpected order: %s, %s", out.Accounts[0].Name, out.Accounts[1].Name)
	}
}

func TestPostTransactionAndBalance(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	rev := createAccount(t, r, "Revenue", "income")

	t.Run("happy path 201", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
			"currency": "USD",
			"postings": []map[string]any{
				{"account_id": cash, "amount": 10000, "description": "sale"},
				{"account_id": rev, "amount": -10000},
			},
		}, map[string]string{"Idempotency-Key": "post-and-balance-1"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("balance reflects the post", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/balance", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		var out struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Amount != 10000 || out.Currency != "USD" {
			t.Errorf("balance = %+v, want 10000 USD", out)
		}
	})

	t.Run("unbalanced 422", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
			"currency": "USD",
			"postings": []map[string]any{
				{"account_id": cash, "amount": 10000},
				{"account_id": rev, "amount": -9999},
			},
		}, map[string]string{"Idempotency-Key": "post-and-balance-unbalanced"})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("too few postings 422", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
			"currency": "USD",
			"postings": []map[string]any{{"account_id": cash, "amount": 0}},
		}, map[string]string{"Idempotency-Key": "post-and-balance-too-few"})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422", rec.Code)
		}
	})

	// ADR-012 "Input hardening": the postings array has a maxItems of 100, so
	// one request can no longer become an arbitrarily large transaction. This
	// is huma schema validation (maxItems), so it rejects before the handler
	// (and the balance check) ever runs. The 101 postings here deliberately DO
	// sum to zero (100 legs of +1 on cash, one leg of -100 on rev): if
	// maxItems were removed, this would be a perfectly valid balanced
	// transaction and the handler would accept it. That is the point: the
	// only thing that can reject this request is the array-length schema
	// check, so asserting on the huma error location below actually pins
	// maxItems instead of coincidentally tripping the unrelated unbalanced
	// check.
	t.Run("too many postings 422", func(t *testing.T) {
		postings := make([]map[string]any, 101)
		for i := 0; i < 100; i++ {
			postings[i] = map[string]any{"account_id": cash, "amount": 1}
		}
		postings[100] = map[string]any{"account_id": rev, "amount": -100}
		rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
			"currency": "USD",
			"postings": postings,
		}, map[string]string{"Idempotency-Key": "post-and-balance-too-many"})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
		var out struct {
			Errors []struct {
				Location string `json:"location"`
				Message  string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal error body: %v (%s)", err, rec.Body.String())
		}
		found := false
		for _, e := range out.Errors {
			if strings.Contains(e.Location, "postings") && strings.Contains(e.Message, "array length") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("errors = %+v, want an entry naming postings and array length (schema maxItems rejection), got body %s", out.Errors, rec.Body.String())
		}
	})

	// ADR-012 "Input hardening": create-transaction sets MaxBodyBytes to
	// MaxRequestBodyBytes (64 KiB), so huma's own body read stops at the limit
	// and returns 413, independent of the router-level body-size middleware in
	// cmd/server (which is not present on this test router).
	t.Run("oversized body 413", func(t *testing.T) {
		body := strings.Repeat("a", int(MaxRequestBodyBytes)+1)
		rec := postJSON(t, r, "/v1/transactions", body, map[string]string{"Idempotency-Key": "oversized-body"})
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status %d, want 413 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing idempotency key 400", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
			"currency": "USD",
			"postings": []map[string]any{
				{"account_id": cash, "amount": 100},
				{"account_id": rev, "amount": -100},
			},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Idempotency-Key header is required") {
			t.Errorf("body = %s, want the missing-key message", rec.Body.String())
		}
	})
}

func TestStatementPagination(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	other := createAccount(t, r, "Other", "asset")

	// Post three transactions so cash has three postings. Each needs its own
	// idempotency key: the bodies are identical, and reusing one key across
	// them would replay the first post instead of creating three.
	for i := 0; i < 3; i++ {
		rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
			"currency": "USD",
			"postings": []map[string]any{
				{"account_id": cash, "amount": 100, "description": "deposit"},
				{"account_id": other, "amount": -100},
			},
		}, map[string]string{"Idempotency-Key": fmt.Sprintf("statement-pagination-%d", i)})
		if rec.Code != http.StatusCreated {
			t.Fatalf("post %d: %d (%s)", i, rec.Code, rec.Body.String())
		}
	}

	// First page of 2, newest first, with running balances 300 then 200.
	rec := do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/statement?limit=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("statement: %d (%s)", rec.Code, rec.Body.String())
	}
	var page1 struct {
		Entries []struct {
			Amount         int64  `json:"amount"`
			RunningBalance int64  `json:"running_balance"`
			Description    string `json:"description"`
		} `json:"entries"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page1.Entries) != 2 {
		t.Fatalf("page1 has %d entries, want 2", len(page1.Entries))
	}
	if page1.Entries[0].RunningBalance != 300 || page1.Entries[1].RunningBalance != 200 {
		t.Errorf("running balances = %d,%d want 300,200", page1.Entries[0].RunningBalance, page1.Entries[1].RunningBalance)
	}
	if page1.NextCursor == nil {
		t.Fatal("expected next_cursor on a full page")
	}

	// Second page: the remaining entry, running balance 100, no further cursor.
	rec = do(t, r, http.MethodGet, "/v1/accounts/"+cash+"/statement?limit=2&cursor="+*page1.NextCursor, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("page2: %d (%s)", rec.Code, rec.Body.String())
	}
	var page2 struct {
		Entries []struct {
			RunningBalance int64 `json:"running_balance"`
		} `json:"entries"`
		NextCursor *string `json:"next_cursor"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page2)
	if len(page2.Entries) != 1 {
		t.Fatalf("page2 has %d entries, want 1", len(page2.Entries))
	}
	if page2.Entries[0].RunningBalance != 100 {
		t.Errorf("page2 running balance = %d, want 100", page2.Entries[0].RunningBalance)
	}
	if page2.NextCursor != nil {
		t.Errorf("expected no next_cursor on the last page, got %q", *page2.NextCursor)
	}
}

func TestCreateTransactionIdempotentReplayHeader(t *testing.T) {
	repo := newFakeRepo()
	a := &domain.Account{Name: "A", Type: domain.Asset, Currency: "USD"}
	b := &domain.Account{Name: "B", Type: domain.Income, Currency: "USD"}
	_ = repo.CreateAccount(context.Background(), "t", a)
	_ = repo.CreateAccount(context.Background(), "t", b)
	router := newAPIRouter(repo)

	body := `{"currency":"USD","postings":[` +
		`{"account_id":"` + a.ID + `","amount":100},` +
		`{"account_id":"` + b.ID + `","amount":-100}]}`

	// First call: 201, no replay header (or "false").
	rec1 := postJSON(t, router, "/v1/transactions", body, map[string]string{"Idempotency-Key": "abc"})
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", rec1.Code)
	}
	var txn1 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &txn1); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	// Retry same key + body: replay header true, same id.
	rec2 := postJSON(t, router, "/v1/transactions", body, map[string]string{"Idempotency-Key": "abc"})
	if rec2.Code != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201", rec2.Code)
	}
	if rec2.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay header = %q, want true", rec2.Header().Get("Idempotent-Replayed"))
	}
	var txn2 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &txn2); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if txn1.ID != txn2.ID {
		t.Errorf("replay returned different id: first=%s, replay=%s", txn1.ID, txn2.ID)
	}

	// Same key, different body: 409.
	other := `{"currency":"USD","postings":[` +
		`{"account_id":"` + a.ID + `","amount":200},` +
		`{"account_id":"` + b.ID + `","amount":-200}]}`
	rec3 := postJSON(t, router, "/v1/transactions", other, map[string]string{"Idempotency-Key": "abc"})
	if rec3.Code != http.StatusConflict {
		t.Errorf("conflict status = %d, want 409", rec3.Code)
	}
}

func TestAuditEndpoints(t *testing.T) {
	repo := newFakeRepo()
	a := &domain.Account{Name: "A", Type: domain.Asset, Currency: "USD"}
	b := &domain.Account{Name: "B", Type: domain.Income, Currency: "USD"}
	_ = repo.CreateAccount(context.Background(), "t", a)
	_ = repo.CreateAccount(context.Background(), "t", b)
	router := newAPIRouter(repo)

	body := `{"currency":"USD","postings":[` +
		`{"account_id":"` + a.ID + `","amount":100},` +
		`{"account_id":"` + b.ID + `","amount":-100}]}`
	rec := postJSON(t, router, "/v1/transactions", body, map[string]string{"Idempotency-Key": "audit-endpoints"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("post status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	byTxn := getJSON(t, router, "/v1/transactions/"+created.ID+"/audit")
	if byTxn.Code != http.StatusOK {
		t.Fatalf("audit by txn status = %d", byTxn.Code)
	}
	if !strings.Contains(byTxn.Body.String(), "transaction.created") {
		t.Errorf("audit by txn body missing action: %s", byTxn.Body.String())
	}
	if !strings.Contains(byTxn.Body.String(), a.ID) {
		t.Errorf("audit by txn body missing account id in after: %s", byTxn.Body.String())
	}
	if !strings.Contains(byTxn.Body.String(), `"currency":"USD"`) {
		t.Errorf("audit by txn body missing currency in after: %s", byTxn.Body.String())
	}

	byAcct := getJSON(t, router, "/v1/accounts/"+a.ID+"/audit")
	if byAcct.Code != http.StatusOK {
		t.Fatalf("audit by account status = %d", byAcct.Code)
	}
	if !strings.Contains(byAcct.Body.String(), "transaction.created") {
		t.Errorf("audit by account body missing action: %s", byAcct.Body.String())
	}
	if !strings.Contains(byAcct.Body.String(), a.ID) {
		t.Errorf("audit by account body missing account id in after: %s", byAcct.Body.String())
	}
	if !strings.Contains(byAcct.Body.String(), `"currency":"USD"`) {
		t.Errorf("audit by account body missing currency in after: %s", byAcct.Body.String())
	}

	// A full page (limit=1 against a single audit row) hands back a next_cursor.
	pagedAcct := getJSON(t, router, "/v1/accounts/"+a.ID+"/audit?limit=1")
	if pagedAcct.Code != http.StatusOK {
		t.Fatalf("audit by account paged status = %d", pagedAcct.Code)
	}
	var page struct {
		Entries    []AuditEntryBody `json:"entries"`
		NextCursor *string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(pagedAcct.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode paged audit: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("paged audit has %d entries, want 1", len(page.Entries))
	}
	if page.NextCursor == nil {
		t.Fatal("expected next_cursor on a full page")
	}
}

// TestMalformedCursorRejected checks that a cursor that does not decode to a
// valid keyset position (decodeCursor's error path in cursor.go) comes back as
// 422 on every endpoint that accepts one, rather than a 500 or a silently
// ignored cursor.
func TestMalformedCursorRejected(t *testing.T) {
	repo := newFakeRepo()
	a := &domain.Account{Name: "A", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(context.Background(), "t", a); err != nil {
		t.Fatalf("create account: %v", err)
	}
	router := newAPIRouter(repo)

	t.Run("statement", func(t *testing.T) {
		rec := do(t, router, http.MethodGet, "/v1/accounts/"+a.ID+"/statement?cursor=not-a-valid-cursor", nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("audit", func(t *testing.T) {
		rec := do(t, router, http.MethodGet, "/v1/accounts/"+a.ID+"/audit?cursor=garbage", nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})
}
