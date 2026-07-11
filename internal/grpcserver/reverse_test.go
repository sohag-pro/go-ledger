package grpcserver_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/sohag-pro/go-ledger/internal/genproto/ledger/v1"
)

// TestGRPCReverseTransaction covers ReverseTransaction end to end: posting a
// transaction, reversing it (negated legs, linked id, AlreadyReversed
// false), reversing again (same reversal, AlreadyReversed true), reversing
// the reversal itself (FailedPrecondition), and reversing an unknown id
// (NotFound).
func TestGRPCReverseTransaction(t *testing.T) {
	client := dialClient(t)
	ctx := authedCtx(context.Background())

	cash, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Cash", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	rev, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Revenue", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}

	postCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "grpc-reverse-post-"+uuid.NewString())
	post, err := client.PostTransaction(postCtx, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: cash.Account.Id, Amount: 10000},
			{AccountId: rev.Account.Id, Amount: -10000},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	first, err := client.ReverseTransaction(ctx, &ledgerv1.ReverseTransactionRequest{OriginalTransactionId: post.Transaction.Id})
	if err != nil {
		t.Fatalf("first reverse: %v", err)
	}
	if first.AlreadyReversed {
		t.Error("first reverse: AlreadyReversed = true, want false")
	}
	if first.Transaction.Id == post.Transaction.Id {
		t.Error("reversal id equals the original id, want a distinct new transaction")
	}
	if first.Transaction.ReversesTransactionId != post.Transaction.Id {
		t.Errorf("ReversesTransactionId = %q, want %q", first.Transaction.ReversesTransactionId, post.Transaction.Id)
	}
	if len(first.Transaction.Postings) != 2 {
		t.Fatalf("reversal postings = %d, want 2", len(first.Transaction.Postings))
	}
	for _, p := range first.Transaction.Postings {
		switch p.AccountId {
		case cash.Account.Id:
			if p.Amount != -10000 {
				t.Errorf("cash posting amount = %d, want -10000", p.Amount)
			}
		case rev.Account.Id:
			if p.Amount != 10000 {
				t.Errorf("revenue posting amount = %d, want 10000", p.Amount)
			}
		}
	}

	cashBal, err := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: cash.Account.Id})
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if cashBal.Amount != 0 {
		t.Errorf("cash balance after reversal = %d, want 0", cashBal.Amount)
	}

	second, err := client.ReverseTransaction(ctx, &ledgerv1.ReverseTransactionRequest{OriginalTransactionId: post.Transaction.Id})
	if err != nil {
		t.Fatalf("second reverse: %v", err)
	}
	if !second.AlreadyReversed {
		t.Error("second reverse: AlreadyReversed = false, want true")
	}
	if second.Transaction.Id != first.Transaction.Id {
		t.Errorf("second reversal id = %s, want %s (the same reversal)", second.Transaction.Id, first.Transaction.Id)
	}

	_, err = client.ReverseTransaction(ctx, &ledgerv1.ReverseTransactionRequest{OriginalTransactionId: first.Transaction.Id})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("reversing a reversal: code = %v, want FailedPrecondition", status.Code(err))
	}

	_, err = client.ReverseTransaction(ctx, &ledgerv1.ReverseTransactionRequest{OriginalTransactionId: uuid.NewString()})
	if status.Code(err) != codes.NotFound {
		t.Errorf("reversing an unknown id: code = %v, want NotFound", status.Code(err))
	}
}
