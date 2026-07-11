package grpcserver_test

// This file covers Task 5.1 (audit A2.2): bringing the gRPC surface to
// parity with REST's rate limiting, request-size cap, and postings-count
// cap. It reuses the bufconn harness (dialClient, dialClientWithLimiter,
// testTenant, sharedPool) from server_test.go rather than building a second
// one.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	ledgerv1 "github.com/sohag-pro/go-ledger/internal/genproto/ledger/v1"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestGRPCPostTransactionRejectsTooManyPostings proves PostTransaction rejects
// a request over maxPostingsPerTransaction (100) with codes.InvalidArgument,
// before any account lookup or balance check: the 101 postings below name
// accounts that do not exist and do not sum to zero, and the call still fails
// on the count, not on those other problems, since the count check runs
// first in the handler.
func TestGRPCPostTransactionRejectsTooManyPostings(t *testing.T) {
	client := dialClient(t)
	ctx := metadata.AppendToOutgoingContext(authedCtx(context.Background()), "idempotency-key", "too-many-postings")

	postings := make([]*ledgerv1.Posting, 101)
	for i := range postings {
		postings[i] = &ledgerv1.Posting{AccountId: fmt.Sprintf("nonexistent-%d", i), Amount: 1}
	}

	_, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{Currency: "USD", Postings: postings})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("101 postings: code = %v, want InvalidArgument", status.Code(err))
	}
	if !strings.Contains(err.Error(), "101") || !strings.Contains(err.Error(), "100") {
		t.Errorf("error = %v, want it to name both the submitted count (101) and the max (100)", err)
	}
}

// TestGRPCPostTransactionAllows100Postings proves exactly
// maxPostingsPerTransaction (100) postings is NOT rejected by the count
// check: domain.Transaction.Validate explicitly allows a transaction to
// touch the same account more than once (see its own doc comment), so 50
// debits of 1 against one account and 50 credits of 1 against another both
// clears the postings cap and balances to zero.
func TestGRPCPostTransactionAllows100Postings(t *testing.T) {
	client := dialClient(t)
	ctx := authedCtx(context.Background())

	a, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Postings Cap A", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create account a: %v", err)
	}
	b, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Postings Cap B", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create account b: %v", err)
	}

	const half = 50
	postings := make([]*ledgerv1.Posting, 0, 2*half)
	for i := 0; i < half; i++ {
		postings = append(postings,
			&ledgerv1.Posting{AccountId: a.Account.Id, Amount: 1},
			&ledgerv1.Posting{AccountId: b.Account.Id, Amount: -1},
		)
	}
	if len(postings) != 100 {
		t.Fatalf("test setup: built %d postings, want 100", len(postings))
	}

	postCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "exactly-100-postings")
	resp, err := client.PostTransaction(postCtx, &ledgerv1.PostTransactionRequest{Currency: "USD", Postings: postings})
	if err != nil {
		t.Fatalf("100 postings should be allowed by the cap: %v", err)
	}
	if len(resp.Transaction.Postings) != 100 {
		t.Errorf("posted transaction has %d postings, want 100", len(resp.Transaction.Postings))
	}
}

// TestGRPCMaxRecvMsgSizeRejectsOversizedMessage proves the server rejects an
// incoming message over maxGRPCRecvMsgBytes (1 MiB) at the transport level,
// replacing gRPC's 4 MiB library default (Task 5.1, audit A2.2). The
// oversized field here is a single posting's free-text Description; the
// request never reaches the handler (and so never reaches the postings-count
// check or PostTransaction's own MaxPostingDescriptionLen validation either),
// since gRPC enforces MaxRecvMsgSize while reading the message off the wire,
// before it is unmarshaled and handed to any interceptor or handler.
func TestGRPCMaxRecvMsgSizeRejectsOversizedMessage(t *testing.T) {
	client := dialClient(t)
	ctx := metadata.AppendToOutgoingContext(authedCtx(context.Background()), "idempotency-key", "oversized-message")

	// 1 MiB + 256 KiB of padding: comfortably over the 1 MiB cap regardless of
	// the rest of the message's (small) overhead.
	oversizedDescription := strings.Repeat("a", (1<<20)+(1<<18))
	_, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: "acct-1", Amount: 1, Description: oversizedDescription},
			{AccountId: "acct-2", Amount: -1},
		},
	})
	if err == nil {
		t.Fatal("expected an error for a message over MaxRecvMsgSize, got nil")
	}
	// grpc-go's transport rejects an oversized incoming message with
	// codes.ResourceExhausted, the same code the rate-limit interceptor
	// uses for a different reason; that overlap is fine here since this test
	// only asserts the transport-level rejection, not which subsystem
	// produced it.
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized message: code = %v, want ResourceExhausted", status.Code(err))
	}
}

// TestGRPCRateLimitOverLimitIsResourceExhausted proves the rate-limit
// interceptor is really wired into the live server chain (not just unit
// tested against the interceptor function directly, which
// TestRateLimitInterceptor* in interceptors_test.go already covers): a key
// dialed against a deliberately tiny limiter gets ResourceExhausted once its
// burst is spent, over a real bufconn round trip through the full
// interceptor chain (recovery, logging, auth, then rate limit).
func TestGRPCRateLimitOverLimitIsResourceExhausted(t *testing.T) {
	client := dialClientWithLimiter(t, auth.NewLimiter(1)) // burst of exactly 1
	ctx := authedCtx(context.Background())

	if _, err := client.ListAccounts(ctx, &ledgerv1.ListAccountsRequest{}); err != nil {
		t.Fatalf("first call within burst: %v", err)
	}
	_, err := client.ListAccounts(ctx, &ledgerv1.ListAccountsRequest{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second call past burst: code = %v, want ResourceExhausted", status.Code(err))
	}
}

// TestGRPCRateLimitIndependentKeysOverBufconn proves two distinct API keys
// dialed against the SAME tiny limiter get independent budgets over a real
// bufconn round trip: exhausting one key's burst does not affect a second,
// separately provisioned key.
func TestGRPCRateLimitIndependentKeysOverBufconn(t *testing.T) {
	limiter := auth.NewLimiter(1) // burst of exactly 1 per key
	client := dialClientWithLimiter(t, limiter)

	// Provision a second tenant and key against the same shared pool the
	// harness already uses, independent of testAPIKeyPlaintext.
	const secondTenant = "00000000-0000-0000-0000-0000000000bb"
	const secondKeyPlaintext = "glk_grpc-server-test-key-2" //nolint:gosec // test fixture key, not a real credential
	repo := postgres.NewRepository(sharedPool)
	if err := repo.CreateTenant(context.Background(), secondTenant, "grpc rate limit second tenant"); err != nil {
		t.Fatalf("create second tenant: %v", err)
	}
	if err := repo.InsertAPIKey(context.Background(),
		domain.APIKey{TenantID: secondTenant, Name: "grpc rate limit second key"},
		domain.HashAPIKey(secondKeyPlaintext),
	); err != nil {
		t.Fatalf("provision second api key: %v", err)
	}

	firstCtx := authedCtx(context.Background())
	secondCtx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+secondKeyPlaintext)

	if _, err := client.ListAccounts(firstCtx, &ledgerv1.ListAccountsRequest{}); err != nil {
		t.Fatalf("key 1 first call: %v", err)
	}
	if _, err := client.ListAccounts(firstCtx, &ledgerv1.ListAccountsRequest{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("key 1 second call: code = %v, want ResourceExhausted", status.Code(err))
	}
	if _, err := client.ListAccounts(secondCtx, &ledgerv1.ListAccountsRequest{}); err != nil {
		t.Fatalf("key 2 first call: err = %v, want nil (independent bucket from key 1)", err)
	}
}
