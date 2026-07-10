package grpcserver_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/sohag-pro/go-ledger/internal/genproto/ledger/v1"
)

// TestGRPCPostTransactionReferenceAndEffectiveAt covers the Task 4.3 (audit
// A1.3) fields end to end over gRPC: reference and effective_at round-trip
// on the created transaction, and a second post reusing the same reference
// for the same tenant is rejected with codes.AlreadyExists (distinct from
// the idempotency-key conflict, which TestGRPCIdempotentReplay already
// covers with its own key reuse).
func TestGRPCPostTransactionReferenceAndEffectiveAt(t *testing.T) {
	client := dialClient(t)
	base := authedCtx(context.Background())

	a, err := client.CreateAccount(base, &ledgerv1.CreateAccountRequest{Name: "GRPC Ref A", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create account a: %v", err)
	}
	b, err := client.CreateAccount(base, &ledgerv1.CreateAccountRequest{Name: "GRPC Ref B", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create account b: %v", err)
	}

	// Stamp a couple seconds in the past, the convention this repo's tests
	// use for a backdated effective_at.
	past := time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano)
	req := &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: a.Account.Id, Amount: 750},
			{AccountId: b.Account.Id, Amount: -750},
		},
		Reference:   "grpc-ref-inv-1001",
		EffectiveAt: past,
	}
	ctx := metadata.AppendToOutgoingContext(base, "idempotency-key", "grpc-reference-1")

	resp, err := client.PostTransaction(ctx, req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.Transaction.Reference != "grpc-ref-inv-1001" {
		t.Errorf("reference = %q, want grpc-ref-inv-1001", resp.Transaction.Reference)
	}
	if resp.Transaction.EffectiveAt != past {
		t.Errorf("effective_at = %q, want %q", resp.Transaction.EffectiveAt, past)
	}

	got, err := client.GetTransaction(base, &ledgerv1.GetTransactionRequest{Id: resp.Transaction.Id})
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if got.Transaction.Reference != "grpc-ref-inv-1001" {
		t.Errorf("re-read reference = %q, want grpc-ref-inv-1001", got.Transaction.Reference)
	}
	if got.Transaction.EffectiveAt != past {
		t.Errorf("re-read effective_at = %q, want %q", got.Transaction.EffectiveAt, past)
	}

	// A second post, different idempotency key, reusing the same reference:
	// must be rejected as AlreadyExists, not replayed and not a plain
	// internal error.
	dupCtx := metadata.AppendToOutgoingContext(base, "idempotency-key", "grpc-reference-2")
	_, err = client.PostTransaction(dupCtx, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: a.Account.Id, Amount: 100},
			{AccountId: b.Account.Id, Amount: -100},
		},
		Reference: "grpc-ref-inv-1001",
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate reference: code = %v, want AlreadyExists (err=%v)", status.Code(err), err)
	}
}

// TestGRPCPostTransactionNoReferenceOrEffectiveAt checks the default path:
// omitting both fields leaves reference empty and gives effective_at a
// non-empty, present-time value (the created_at fallback), instead of an
// empty string that would be indistinguishable from a malformed timestamp.
func TestGRPCPostTransactionNoReferenceOrEffectiveAt(t *testing.T) {
	client := dialClient(t)
	base := authedCtx(context.Background())

	a, err := client.CreateAccount(base, &ledgerv1.CreateAccountRequest{Name: "GRPC NoRef A", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create account a: %v", err)
	}
	b, err := client.CreateAccount(base, &ledgerv1.CreateAccountRequest{Name: "GRPC NoRef B", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create account b: %v", err)
	}

	ctx := metadata.AppendToOutgoingContext(base, "idempotency-key", "grpc-no-reference-1")
	resp, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: a.Account.Id, Amount: 250},
			{AccountId: b.Account.Id, Amount: -250},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.Transaction.Reference != "" {
		t.Errorf("reference = %q, want empty", resp.Transaction.Reference)
	}
	if resp.Transaction.EffectiveAt == "" {
		t.Fatal("effective_at is empty, want the created_at fallback")
	}
	if _, err := time.Parse(time.RFC3339Nano, resp.Transaction.EffectiveAt); err != nil {
		t.Errorf("effective_at = %q is not a valid RFC3339 timestamp: %v", resp.Transaction.EffectiveAt, err)
	}
}
