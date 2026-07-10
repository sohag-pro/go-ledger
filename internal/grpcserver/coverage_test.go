package grpcserver_test

// This file rounds out internal/grpcserver coverage: the handlers that
// server_test.go does not exercise (GetAccount, ListAccounts, GetStatement,
// GetTransaction, GetTransactionAudit, GetAccountAudit), their error branches,
// and the tenant/idempotency-key defaults that only surface when a handler is
// called without the interceptor chain in front of it. It reuses the
// bufconn harness (dialClient, testTenant, sharedPool) from server_test.go
// rather than building a second one.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	ledgerv1 "github.com/sohag-pro/go-ledger/internal/genproto/ledger/v1"
	"github.com/sohag-pro/go-ledger/internal/grpcserver"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

const missingID = "00000000-0000-0000-0000-000000000000"

// newDepsAndServer builds the same Deps dialClient uses, plus the raw
// *grpcserver.Server (no grpc.Server, no bufconn, no interceptor chain). Tests
// that need to observe a handler's behavior when called directly, without the
// auth interceptor injecting a tenant, use it instead of dialClient.
func newDepsAndServer(t *testing.T) (grpcserver.Deps, *grpcserver.Server) {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	repo := postgres.NewRepository(sharedPool)
	deps := grpcserver.Deps{
		Accounts:     ledger.NewAccountService(repo),
		Transactions: ledger.NewTransactionService(repo, nil, nil),
		Audit:        ledger.NewAuditService(repo),
		Auth:         auth.NewResolver(repo, time.Minute),
	}
	return deps, grpcserver.NewServer(deps)
}

func TestGRPCAccountLifecycle(t *testing.T) {
	client := dialClient(t)
	ctx := authedCtx(context.Background())

	created, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Coverage Checking", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	got, err := client.GetAccount(ctx, &ledgerv1.GetAccountRequest{Id: created.Account.Id})
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Account.Id != created.Account.Id || got.Account.Name != "Coverage Checking" ||
		got.Account.Type != "asset" || got.Account.Currency != "USD" {
		t.Errorf("get account = %+v, want a match for created %+v", got.Account, created.Account)
	}

	if _, err := client.GetAccount(ctx, &ledgerv1.GetAccountRequest{Id: missingID}); status.Code(err) != codes.NotFound {
		t.Errorf("get missing account code = %v, want NotFound", status.Code(err))
	}

	list, err := client.ListAccounts(ctx, &ledgerv1.ListAccountsRequest{})
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	found := false
	for _, a := range list.Accounts {
		if a.Id == created.Account.Id {
			found = true
			break
		}
	}
	if !found {
		t.Error("list accounts did not include the account just created")
	}
}

func TestGRPCCreateAccountInvalidType(t *testing.T) {
	client := dialClient(t)
	_, err := client.CreateAccount(authedCtx(context.Background()), &ledgerv1.CreateAccountRequest{Name: "Bad Type", Type: "not-a-type", Currency: "USD"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCCreateAccountInvalidAccount(t *testing.T) {
	client := dialClient(t)
	_, err := client.CreateAccount(authedCtx(context.Background()), &ledgerv1.CreateAccountRequest{Name: "", Type: "asset", Currency: "USD"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCGetBalanceNotFound(t *testing.T) {
	client := dialClient(t)
	_, err := client.GetBalance(authedCtx(context.Background()), &ledgerv1.GetBalanceRequest{AccountId: missingID})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestGRPCPostTransactionInvalidCurrency(t *testing.T) {
	client := dialClient(t)
	ctx := metadata.AppendToOutgoingContext(authedCtx(context.Background()), "idempotency-key", "bad-currency")
	a, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Bad Currency A", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Bad Currency B", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	_, err = client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{
		Currency: "usd", // lowercase fails domain.Currency.Validate
		Postings: []*ledgerv1.Posting{
			{AccountId: a.Account.Id, Amount: 100},
			{AccountId: b.Account.Id, Amount: -100},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

// TestGRPCPostTransactionIdempotencyMetadataWithoutKey sends outgoing
// metadata that does not include an idempotency-key. On the server side this
// makes metadata.FromIncomingContext report ok=true with an empty lookup, the
// idempotencyKeyFrom branch that a bare request (no metadata at all) does not
// reach. Since the idempotency key is mandatory (ADR-012), this is still
// rejected, as InvalidArgument, distinctly from the "no metadata at all"
// case exercised in TestGRPCHandlersWithoutInterceptorChain.
func TestGRPCPostTransactionIdempotencyMetadataWithoutKey(t *testing.T) {
	client := dialClient(t)
	ctx := metadata.AppendToOutgoingContext(authedCtx(context.Background()), "trace-id", "abc123")

	a, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "No Idem A", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "No Idem B", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	_, err = client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: a.Account.Id, Amount: 250},
			{AccountId: b.Account.Id, Amount: -250},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCGetTransaction(t *testing.T) {
	client := dialClient(t)
	ctx := authedCtx(context.Background())
	a, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Get Txn A", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Get Txn B", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	postCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "get-transaction")
	post, err := client.PostTransaction(postCtx, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: a.Account.Id, Amount: 777, Description: "coverage"},
			{AccountId: b.Account.Id, Amount: -777},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	got, err := client.GetTransaction(ctx, &ledgerv1.GetTransactionRequest{Id: post.Transaction.Id})
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if got.Transaction.Id != post.Transaction.Id || len(got.Transaction.Postings) != 2 {
		t.Errorf("get transaction = %+v, want a match for posted %+v", got.Transaction, post.Transaction)
	}
	// Transaction has no transaction-level currency field (ADR-014, reworked
	// the same way as REST): each posting carries its own currency instead.
	for _, p := range got.Transaction.Postings {
		if p.Currency != "USD" {
			t.Errorf("posting currency = %q, want USD", p.Currency)
		}
	}

	if _, err := client.GetTransaction(ctx, &ledgerv1.GetTransactionRequest{Id: missingID}); status.Code(err) != codes.NotFound {
		t.Errorf("get missing transaction code = %v, want NotFound", status.Code(err))
	}
}

func TestGRPCGetTransactionAudit(t *testing.T) {
	client := dialClient(t)
	ctx := authedCtx(context.Background())
	a, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Txn Audit A", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Txn Audit B", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	postCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "get-transaction-audit")
	post, err := client.PostTransaction(postCtx, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: a.Account.Id, Amount: 400},
			{AccountId: b.Account.Id, Amount: -400},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	// PostTransaction only writes an audit_outbox row (ADR-017); drain the
	// chainer so there is an audit_log row to read back.
	drainChainer(t, sharedPool, testTenant)

	resp, err := client.GetTransactionAudit(ctx, &ledgerv1.GetTransactionAuditRequest{TransactionId: post.Transaction.Id})
	if err != nil {
		t.Fatalf("transaction audit: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(resp.Entries))
	}
	entry := resp.Entries[0]
	if entry.Action != domain.ActionTransactionCreated {
		t.Errorf("action = %q, want %q", entry.Action, domain.ActionTransactionCreated)
	}
	if entry.TransactionId != post.Transaction.Id {
		t.Errorf("transaction id = %q, want %q", entry.TransactionId, post.Transaction.Id)
	}
	if entry.Before != "" {
		t.Errorf("before = %q, want empty for a create", entry.Before)
	}
	if entry.After == "" {
		t.Error("after snapshot should not be empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, entry.CreatedAt); err != nil {
		t.Errorf("created_at %q is not RFC3339Nano: %v", entry.CreatedAt, err)
	}
}

func TestGRPCGetStatementPaginatesAndValidatesCursor(t *testing.T) {
	client := dialClient(t)
	ctx := authedCtx(context.Background())
	cash, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Statement Cash", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	rev, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Statement Revenue", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}

	// Each post needs its own idempotency key: the bodies are identical, and
	// reusing one key across them would replay the first post instead of
	// creating two.
	for i := 0; i < 2; i++ {
		postCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", fmt.Sprintf("get-statement-%d", i))
		if _, err := client.PostTransaction(postCtx, &ledgerv1.PostTransactionRequest{
			Currency: "USD",
			Postings: []*ledgerv1.Posting{
				{AccountId: cash.Account.Id, Amount: 100, Description: "coverage statement"},
				{AccountId: rev.Account.Id, Amount: -100},
			},
		}); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}

	page1, err := client.GetStatement(ctx, &ledgerv1.GetStatementRequest{AccountId: cash.Account.Id, Limit: 1})
	if err != nil {
		t.Fatalf("statement page1: %v", err)
	}
	if len(page1.Entries) != 1 {
		t.Fatalf("page1 entries = %d, want 1", len(page1.Entries))
	}
	if page1.AccountId != cash.Account.Id || page1.Currency != "USD" {
		t.Errorf("page1 account/currency = %s/%s, want %s/USD", page1.AccountId, page1.Currency, cash.Account.Id)
	}
	if page1.NextCursor == "" {
		t.Fatal("expected a next_cursor when the page is full")
	}

	if _, err := client.GetStatement(ctx, &ledgerv1.GetStatementRequest{
		AccountId: cash.Account.Id,
		Cursor:    "not valid base64 !!!",
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid cursor code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCGetAccountAuditPaginatesAndValidatesCursor(t *testing.T) {
	client := dialClient(t)
	ctx := authedCtx(context.Background())
	cash, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Audit Cash", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	rev, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Audit Revenue", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}

	// Each post needs its own idempotency key: the bodies are identical, and
	// reusing one key across them would replay the first post instead of
	// creating two.
	for i := 0; i < 2; i++ {
		postCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", fmt.Sprintf("get-account-audit-%d", i))
		if _, err := client.PostTransaction(postCtx, &ledgerv1.PostTransactionRequest{
			Currency: "USD",
			Postings: []*ledgerv1.Posting{
				{AccountId: cash.Account.Id, Amount: 100},
				{AccountId: rev.Account.Id, Amount: -100},
			},
		}); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	// PostTransaction only writes an audit_outbox row (ADR-017); drain the
	// chainer so there are audit_log rows to page through.
	drainChainer(t, sharedPool, testTenant)

	page1, err := client.GetAccountAudit(ctx, &ledgerv1.GetAccountAuditRequest{AccountId: cash.Account.Id, Limit: 1})
	if err != nil {
		t.Fatalf("account audit page1: %v", err)
	}
	if len(page1.Entries) != 1 {
		t.Fatalf("page1 entries = %d, want 1", len(page1.Entries))
	}
	if page1.Entries[0].Action != domain.ActionTransactionCreated {
		t.Errorf("action = %q, want %q", page1.Entries[0].Action, domain.ActionTransactionCreated)
	}
	if page1.NextCursor == "" {
		t.Fatal("expected a next_cursor when the page is full")
	}

	if _, err := client.GetAccountAudit(ctx, &ledgerv1.GetAccountAuditRequest{
		AccountId: cash.Account.Id,
		Cursor:    "not valid base64 !!!",
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid cursor code = %v, want InvalidArgument", status.Code(err))
	}
}

// TestGRPCNewGRPCServerDefaultsNilLogger exercises the log == nil branch of
// NewGRPCServer, which server_test.go never reaches because dialClient always
// passes a real logger.
func TestGRPCNewGRPCServerDefaultsNilLogger(t *testing.T) {
	deps, _ := newDepsAndServer(t)
	srv := grpcserver.NewGRPCServer(deps, nil)
	if srv == nil {
		t.Fatal("NewGRPCServer(deps, nil) returned a nil server")
	}
	srv.Stop()
}

// TestGRPCHandlersWithoutInterceptorChain calls handler methods directly on a
// *grpcserver.Server, bypassing the tenant and other interceptors that
// dialClient's bufconn server always installs. That is the only way to
// observe tenantFrom's zero-value default ("" when no tenant is on the
// context) and idempotencyKeyFrom's "no incoming metadata at all" branch,
// since a real interceptor chain always injects a tenant and a real gRPC
// server context always carries transport metadata. With no tenant, "" is not
// a valid tenant UUID, so the repository rejects it before it can look
// anything up, and toStatus has no matching domain sentinel for that parse
// failure, so every one of these calls surfaces as Internal rather than
// NotFound. This also exercises each handler's toStatus error branch, since
// ListAuditByTransaction/ListAuditByAccount return an empty page rather than
// an error for an unknown transaction or account, so a well-formed tenant
// never reaches that branch.
//
// PostTransaction is exercised twice: once with no incoming metadata at all
// (idempotencyKeyFrom's !ok branch), which the mandatory check now rejects
// before ever reaching tenantFrom, as InvalidArgument; and once with
// idempotency-key metadata attached directly via metadata.NewIncomingContext
// (bypassing the interceptor chain but not the mandatory check), which reaches
// the same Internal-on-missing-tenant behavior as the other handlers above.
func TestGRPCHandlersWithoutInterceptorChain(t *testing.T) {
	_, s := newDepsAndServer(t)
	bg := context.Background()

	if _, err := s.GetAccount(bg, &ledgerv1.GetAccountRequest{Id: missingID}); status.Code(err) != codes.Internal {
		t.Errorf("GetAccount without a tenant code = %v, want Internal", status.Code(err))
	}
	if _, err := s.ListAccounts(bg, &ledgerv1.ListAccountsRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("ListAccounts without a tenant code = %v, want Internal", status.Code(err))
	}
	if _, err := s.GetStatement(bg, &ledgerv1.GetStatementRequest{AccountId: missingID}); status.Code(err) != codes.Internal {
		t.Errorf("GetStatement without a tenant code = %v, want Internal", status.Code(err))
	}
	if _, err := s.GetTransactionAudit(bg, &ledgerv1.GetTransactionAuditRequest{TransactionId: missingID}); status.Code(err) != codes.Internal {
		t.Errorf("GetTransactionAudit without a tenant code = %v, want Internal", status.Code(err))
	}
	if _, err := s.GetAccountAudit(bg, &ledgerv1.GetAccountAuditRequest{AccountId: missingID}); status.Code(err) != codes.Internal {
		t.Errorf("GetAccountAudit without a tenant code = %v, want Internal", status.Code(err))
	}

	if _, err := s.PostTransaction(bg, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: missingID, Amount: 100},
			{AccountId: missingID, Amount: -100},
		},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("PostTransaction with no metadata at all code = %v, want InvalidArgument", status.Code(err))
	}

	withKey := metadata.NewIncomingContext(bg, metadata.Pairs("idempotency-key", "no-interceptor-chain"))
	_, err := s.PostTransaction(withKey, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: missingID, Amount: 100},
			{AccountId: missingID, Amount: -100},
		},
	})
	if status.Code(err) != codes.Internal {
		t.Errorf("PostTransaction without a tenant code = %v, want Internal", status.Code(err))
	}
}
