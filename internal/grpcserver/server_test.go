package grpcserver_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
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

// testAPIKeyPlaintext is the bearer token every integration test in this file
// authenticates with. dialClient provisions it against testTenant on the
// real Postgres repository before the server starts, so the tests exercise
// the real auth interceptor rather than bypassing it.
const testAPIKeyPlaintext = "glk_grpc-server-test-key" //nolint:gosec // test fixture key, not a real credential

// authedCtx returns ctx with the test API key attached as gRPC authorization
// metadata, the same shape a real client sends: "Bearer <token>".
func authedCtx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+testAPIKeyPlaintext)
}

// provisionTestAPIKeyOnce inserts testAPIKeyPlaintext for testTenant exactly
// once per test binary run: dialClient is called from many test functions
// sharing the same Postgres container, and a second insert of the same key
// hash would violate the api_keys unique constraint.
var provisionTestAPIKeyOnce sync.Once

// testDefaultRateLimitRPM is the rpm dialClient's default rate limiter uses:
// deliberately far higher than any single test's call count (mirroring
// loadTestAPIKeyRateLimitRPM in cmd/server/main.go), so ordinary handler and
// integration tests never trip the rate-limit interceptor by accident. Tests
// that specifically exercise rate limiting use dialClientWithLimiter with
// their own tight limiter instead.
const testDefaultRateLimitRPM = 1_000_000

// dialClient starts the real gRPC server on a bufconn and returns a connected
// generated client plus a cleanup func. opts is passed through to
// ledger.NewTransactionService, e.g. ledger.WithFXProvider(...) for tests that
// exercise Convert (which errors with ledger.ErrNoFXProvider without one). It
// is dialClientWithLimiter with a generous default limiter that no ordinary
// test can exhaust; see testDefaultRateLimitRPM.
func dialClient(t *testing.T, opts ...ledger.ServiceOption) ledgerv1.LedgerServiceClient {
	t.Helper()
	return dialClientWithLimiter(t, auth.NewLimiter(testDefaultRateLimitRPM), opts...)
}

// dialClientWithLimiter is dialClient but with an explicit *auth.Limiter, so
// tests exercising the gRPC rate-limit interceptor (Task 5.1, audit A2.2) can
// wire a tight rpm and drive it to codes.ResourceExhausted through a real
// bufconn round trip, the same interceptor chain cmd/server/main.go wires in
// production.
func dialClientWithLimiter(t *testing.T, limiter *auth.Limiter, opts ...ledger.ServiceOption) ledgerv1.LedgerServiceClient {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	repo := postgres.NewRepository(sharedPool)
	var provisionErr error
	provisionTestAPIKeyOnce.Do(func() {
		if provisionErr = repo.CreateTenant(context.Background(), testTenant, "grpc server test tenant"); provisionErr != nil {
			return
		}
		provisionErr = repo.InsertAPIKey(context.Background(),
			domain.APIKey{TenantID: testTenant, Name: "grpc server test key"},
			domain.HashAPIKey(testAPIKeyPlaintext),
		)
	})
	if provisionErr != nil {
		t.Fatalf("provision test api key: %v", provisionErr)
	}
	deps := grpcserver.Deps{
		Accounts:     ledger.NewAccountService(repo),
		Transactions: ledger.NewTransactionService(repo, nil, nil, opts...),
		Audit:        ledger.NewAuditService(repo),
		Auth:         auth.NewResolver(repo, time.Minute),
		RateLimiter:  limiter,
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

func TestGRPCWithoutAPIKeyIsUnauthenticated(t *testing.T) {
	client := dialClient(t)
	_, err := client.CreateAccount(context.Background(), &ledgerv1.CreateAccountRequest{Name: "Cash", Type: "asset", Currency: "USD"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
}

func TestGRPCPostAndBalance(t *testing.T) {
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

	postCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", "post-and-balance")
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
	base := authedCtx(context.Background())

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

// TestGRPCListTransactions covers the ListTransactions RPC end to end
// against real Postgres (Task 4.4, audit A7.2), mirroring GET
// /v1/transactions: an exact reference match, and a from/to range that
// keyset-paginates via cursor with no gap or overlap and terminates.
//
// This package's tests share one tenant across the whole binary run and do
// not run t.Parallel() against it (see chainer_helper_test.go's doc
// comment), but ListTransactions with no filter would still see every
// transaction any OTHER test in this file has ever posted to that tenant.
// The from/to range this test uses is captured tightly around its own
// PostTransaction calls, which is enough to isolate it from the rest of the
// suite precisely because nothing else runs concurrently.
func TestGRPCListTransactions(t *testing.T) {
	client := dialClient(t)
	ctx := authedCtx(context.Background())

	a, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "GRPC List A", Type: "asset", Currency: "USD"})
	if err != nil {
		t.Fatalf("create account a: %v", err)
	}
	b, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Name: "GRPC List B", Type: "income", Currency: "USD"})
	if err != nil {
		t.Fatalf("create account b: %v", err)
	}

	const n = 5
	const refValue = "grpc-list-ref-2"
	ids := make([]string, n)
	var refTxnID string
	var firstEffectiveAt, lastEffectiveAt string

	for i := 0; i < n; i++ {
		idemCtx := metadata.AppendToOutgoingContext(ctx, "idempotency-key", fmt.Sprintf("grpc-list-seed-%d", i))
		ref := fmt.Sprintf("grpc-list-ref-%d", i)
		resp, err := client.PostTransaction(idemCtx, &ledgerv1.PostTransactionRequest{
			Currency: "USD",
			Postings: []*ledgerv1.Posting{
				{AccountId: a.Account.Id, Amount: int64(100 + i)},
				{AccountId: b.Account.Id, Amount: -int64(100 + i)},
			},
			Reference: ref,
		})
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		ids[i] = resp.Transaction.Id
		if ref == refValue {
			refTxnID = resp.Transaction.Id
		}
		// effective_at defaults to created_at when omitted (Task 4.3, audit
		// A1.3), which every post here does: this is the actual DATABASE
		// SERVER's created_at value for this row, read back from the post
		// response itself. Using it (rather than the test process's own
		// time.Now()) to build the from/to bounds below means the range is
		// immune to any clock skew between this host and the Postgres
		// container.
		if i == 0 {
			firstEffectiveAt = resp.Transaction.EffectiveAt
		}
		lastEffectiveAt = resp.Transaction.EffectiveAt
		time.Sleep(2 * time.Millisecond)
	}

	firstAt, err := time.Parse(time.RFC3339Nano, firstEffectiveAt)
	if err != nil {
		t.Fatalf("parse first effective_at: %v", err)
	}
	lastAt, err := time.Parse(time.RFC3339Nano, lastEffectiveAt)
	if err != nil {
		t.Fatalf("parse last effective_at: %v", err)
	}
	fromStr := firstAt.Format(time.RFC3339Nano) // inclusive: exactly the oldest row's own created_at
	// Exclusive bound one microsecond past the newest row's own created_at
	// (Postgres timestamptz's own resolution): just enough to include it
	// without widening the window far enough to risk catching a transaction
	// from whatever test happens to run next in this shared tenant.
	toStr := lastAt.Add(time.Microsecond).Format(time.RFC3339Nano)

	t.Run("reference filter narrows to exact match", func(t *testing.T) {
		resp, err := client.ListTransactions(ctx, &ledgerv1.ListTransactionsRequest{Reference: refValue})
		if err != nil {
			t.Fatalf("list by reference: %v", err)
		}
		if len(resp.Transactions) != 1 || resp.Transactions[0].Id != refTxnID {
			t.Fatalf("reference filter = %+v, want exactly %s", resp.Transactions, refTxnID)
		}
	})

	t.Run("from/to range paginates the seeded batch with no gap or overlap", func(t *testing.T) {
		const pageSize = 2
		seen := map[string]bool{}
		var walked []string
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > n {
				t.Fatalf("pagination did not terminate after %d pages", pages)
			}
			resp, err := client.ListTransactions(ctx, &ledgerv1.ListTransactionsRequest{
				From: fromStr, To: toStr, Limit: pageSize, Cursor: cursor,
			})
			if err != nil {
				t.Fatalf("list page %d: %v", pages, err)
			}
			for _, txn := range resp.Transactions {
				if seen[txn.Id] {
					t.Fatalf("transaction %s returned twice across pages (overlap)", txn.Id)
				}
				seen[txn.Id] = true
				walked = append(walked, txn.Id)
			}
			if resp.NextCursor == "" {
				break
			}
			cursor = resp.NextCursor
		}
		wantOrder := make([]string, n)
		for i := 0; i < n; i++ {
			wantOrder[i] = ids[n-1-i]
		}
		if !reflect.DeepEqual(walked, wantOrder) {
			t.Fatalf("walked order = %v, want %v (no gap, no overlap, newest first)", walked, wantOrder)
		}
	})
}
