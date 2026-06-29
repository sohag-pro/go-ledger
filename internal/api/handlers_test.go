package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

const testTenant = "00000000-0000-0000-0000-000000000001"

// fakeRepo is an in-memory domain.Repository for handler tests: no database, no
// concurrency semantics, just enough to exercise the HTTP layer end to end.
type fakeRepo struct {
	accounts map[string]domain.Account
	txns     map[string]domain.Transaction
	postings []postingRec
	clock    int64
}

type postingRec struct {
	id, txnID, accountID, description string
	amount                            int64
	createdAt                         time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{accounts: map[string]domain.Account{}, txns: map[string]domain.Transaction{}}
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

var _ domain.Repository = (*fakeRepo)(nil)

func newAPIRouter(repo domain.Repository) chi.Router {
	r := chi.NewRouter()
	New(r, Deps{
		Accounts:      ledger.NewAccountService(repo),
		Transactions:  ledger.NewTransactionService(repo, slog.New(slog.NewTextHandler(io.Discard, nil))),
		DefaultTenant: testTenant,
	})
	return r
}

func do(t *testing.T, r chi.Router, method, path string, body any) *httptest.ResponseRecorder {
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
		})
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
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("too few postings 422", func(t *testing.T) {
		rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
			"currency": "USD",
			"postings": []map[string]any{{"account_id": cash, "amount": 0}},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %d, want 422", rec.Code)
		}
	})
}

func TestStatementPagination(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	other := createAccount(t, r, "Other", "asset")

	// Post three transactions so cash has three postings.
	for i := 0; i < 3; i++ {
		rec := do(t, r, http.MethodPost, "/v1/transactions", map[string]any{
			"currency": "USD",
			"postings": []map[string]any{
				{"account_id": cash, "amount": 100, "description": "deposit"},
				{"account_id": other, "amount": -100},
			},
		})
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
