// Command grpc-client is a small example that exercises the go-ledger gRPC API:
// it creates two accounts, posts a balanced transaction between them, and reads
// a balance back. Run the server first (GRPC_ADDR defaults to :9091), then:
//
//	go run ./examples/grpc-client
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ledgerv1 "github.com/sohag-pro/go-ledger/internal/genproto/ledger/v1"
)

func main() {
	addr := os.Getenv("GRPC_ADDR")
	if addr == "" {
		addr = "localhost:9091"
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial %s: %v", addr, err) //nolint:gosec
	}
	defer func() { _ = conn.Close() }()
	client := ledgerv1.NewLedgerServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cash, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Cash", Type: "asset", Currency: "USD"})
	if err != nil {
		log.Fatalf("create cash: %v", err)
	}
	rev, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Revenue", Type: "income", Currency: "USD"})
	if err != nil {
		log.Fatalf("create revenue: %v", err)
	}

	post, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: cash.Account.Id, Amount: 10000},
			{AccountId: rev.Account.Id, Amount: -10000},
		},
	})
	if err != nil {
		log.Fatalf("post transaction: %v", err)
	}

	bal, err := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: cash.Account.Id})
	if err != nil {
		log.Fatalf("get balance: %v", err)
	}

	fmt.Printf("posted transaction %s (replayed=%v); cash balance = %d %s\n",
		post.Transaction.Id, post.Replayed, bal.Amount, bal.Currency)
}
