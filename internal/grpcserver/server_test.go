package grpcserver_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	ledgerv1 "github.com/sohag-pro/go-ledger/internal/genproto/ledger/v1"
	"github.com/sohag-pro/go-ledger/internal/grpcserver"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

var (
	sharedPool *pgxpool.Pool
	poolErr    error
)

func TestMain(m *testing.M) {
	os.Exit(runWithContainer(m))
}

func runWithContainer(m *testing.M) int {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(120*time.Second)),
	)
	if err != nil {
		poolErr = fmt.Errorf("cannot start postgres (is Docker running?): %w", err)
		return m.Run()
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		poolErr = err
		return m.Run()
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		poolErr = err
		return m.Run()
	}
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		poolErr = err
		_ = db.Close()
		return m.Run()
	}
	if err := goose.Up(db, "migrations"); err != nil {
		poolErr = err
		_ = db.Close()
		return m.Run()
	}
	_ = db.Close()
	pool, err := postgres.NewPool(ctx, dsn, 10)
	if err != nil {
		poolErr = err
		return m.Run()
	}
	defer pool.Close()
	sharedPool = pool
	return m.Run()
}

const testTenant = "00000000-0000-0000-0000-0000000000aa"

// dialClient starts the real gRPC server on a bufconn and returns a connected
// generated client plus a cleanup func.
func dialClient(t *testing.T) ledgerv1.LedgerServiceClient {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	repo := postgres.NewRepository(sharedPool)
	deps := grpcserver.Deps{
		Accounts:      ledger.NewAccountService(repo),
		Transactions:  ledger.NewTransactionService(repo, nil, nil),
		Audit:         ledger.NewAuditService(repo),
		DefaultTenant: testTenant,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := grpcserver.NewGRPCServer(deps, log)

	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(); srv.GracefulStop() })
	return ledgerv1.NewLedgerServiceClient(conn)
}

func TestGRPCPostAndBalance(t *testing.T) {
	client := dialClient(t)
	ctx := context.Background()

	cash, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Cash", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	rev, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "Revenue", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}

	post, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: cash.Account.Id, Amount: 10000},
			{AccountId: rev.Account.Id, Amount: -10000},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if post.Replayed {
		t.Error("first post should not be a replay")
	}

	cashBal, err := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: cash.Account.Id})
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if cashBal.Amount != 10000 {
		t.Errorf("cash balance = %d, want 10000", cashBal.Amount)
	}
}

func TestGRPCIdempotentReplay(t *testing.T) {
	client := dialClient(t)
	base := context.Background()

	a, _ := client.CreateAccount(base, &ledgerv1.CreateAccountRequest{Name: "A", Type: "asset", Currency: "USD"})
	b, _ := client.CreateAccount(base, &ledgerv1.CreateAccountRequest{Name: "B", Type: "income", Currency: "USD"})
	req := &ledgerv1.PostTransactionRequest{
		Currency: "USD",
		Postings: []*ledgerv1.Posting{
			{AccountId: a.Account.Id, Amount: 500},
			{AccountId: b.Account.Id, Amount: -500},
		},
	}
	ctx := metadata.AppendToOutgoingContext(base, "idempotency-key", "grpc-key-1")

	first, err := client.PostTransaction(ctx, req)
	if err != nil {
		t.Fatalf("first post: %v", err)
	}
	second, err := client.PostTransaction(ctx, req)
	if err != nil {
		t.Fatalf("second post: %v", err)
	}
	if !second.Replayed {
		t.Error("second post with same key should be a replay")
	}
	if first.Transaction.Id != second.Transaction.Id {
		t.Errorf("replay returned id %s, want %s", second.Transaction.Id, first.Transaction.Id)
	}
}
