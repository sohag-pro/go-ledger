package ledger_test

// Chaos tests for the two failure modes ADR-011 calls out: a connection lost
// between BEGIN and COMMIT (must roll back cleanly, leaving nothing behind),
// and the ambiguous-commit case a client sees after a lost acknowledgement
// (must not double-post on retry). Both run against a real Postgres reached
// through a toxiproxy container, so the injection happens at the network
// layer rather than by faking an error inside the code under test.
//
// These tests intentionally do not call t.Parallel(): the goose package used
// by the shared migrate() helper (defined in stress_test.go) keeps its
// dialect and base FS in unsynchronized package-level state, so calling it
// from two goroutines at once would be a data race even though both calls set
// the same value. Running serially (the default without t.Parallel()) keeps
// go test -race clean without weakening the test.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	toxiproxyclient "github.com/Shopify/toxiproxy/v2/client"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tctoxiproxy "github.com/testcontainers/testcontainers-go/modules/toxiproxy"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// chaosEnv is the topology one chaos test runs against: a Postgres container
// and a toxiproxy container on a private Docker network, with a proxy named
// "pg" forwarding to Postgres. proxyDSN is the app-facing connection string:
// every pool built from it talks to Postgres only through the proxy, so
// disabling the proxy is a real network cut, not a simulated one.
type chaosEnv struct {
	proxy    *toxiproxyclient.Proxy
	proxyDSN string
}

// startChaosEnv brings up the topology for one chaos test and registers
// cleanup. It skips the test (rather than failing it) if Docker is not
// available, matching the rest of the suite's Docker-gating convention.
// Migrations run over the Postgres container's own directly mapped port,
// never through the proxy, so schema setup itself is never subject to
// injection.
func startChaosEnv(t *testing.T) chaosEnv {
	t.Helper()
	ctx := context.Background()

	nw, err := network.New(ctx)
	if err != nil {
		t.Skipf("chaos test: cannot create docker network (is Docker running?): %v", err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })

	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		network.WithNetwork([]string{"pg"}, nw),
		// Same readiness wait as the rest of the suite: Postgres opens 5432
		// during initdb and restarts it, so the log is watched twice rather
		// than trusting the port to open once.
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		t.Skipf("chaos test: cannot start postgres container (is Docker running?): %v", err)
	}
	t.Cleanup(func() { _ = pgContainer.Terminate(context.Background()) })

	directDSN, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("chaos test: connection string: %v", err)
	}
	// Direct connection, bypassing the proxy entirely: schema setup must not
	// be subject to the injection under test.
	if err := migrate(directDSN); err != nil {
		t.Fatalf("chaos test: migrate: %v", err)
	}

	proxyContainer, err := tctoxiproxy.Run(ctx,
		"ghcr.io/shopify/toxiproxy:2.12.0",
		tctoxiproxy.WithProxy("pg", "pg:5432"),
		network.WithNetwork([]string{"toxiproxy"}, nw),
	)
	if err != nil {
		t.Skipf("chaos test: cannot start toxiproxy container (is Docker running?): %v", err)
	}
	t.Cleanup(func() { _ = proxyContainer.Terminate(context.Background()) })

	toxiURI, err := proxyContainer.URI(ctx)
	if err != nil {
		t.Fatalf("chaos test: toxiproxy uri: %v", err)
	}
	tclient := toxiproxyclient.NewClient(toxiURI)
	proxies, err := tclient.Proxies()
	if err != nil {
		t.Fatalf("chaos test: list proxies: %v", err)
	}
	proxy, ok := proxies["pg"]
	if !ok {
		t.Fatalf("chaos test: proxy %q not found among %v", "pg", proxies)
	}

	proxiedHost, proxiedPort, err := proxyContainer.ProxiedEndpoint(8666)
	if err != nil {
		t.Fatalf("chaos test: proxied endpoint: %v", err)
	}

	return chaosEnv{
		proxy:    proxy,
		proxyDSN: fmt.Sprintf("postgres://ledger:ledger@%s:%s/ledger?sslmode=disable", proxiedHost, proxiedPort),
	}
}

// commitInterceptTracer is a pgx.QueryTracer that fires onCommit the first
// time it observes the literal SQL statement "commit". pgx's Tx.Commit sends
// exactly that statement (jackc/pgx/v5 tx.go, dbTx.Commit: commandSQL :=
// "commit"), and TraceQueryStart runs before the statement is written to the
// wire (jackc/pgx/v5 conn.go, Conn.Exec). Firing here, rather than racing a
// goroutine against wall-clock timing, is what makes Case A deterministic:
// every statement inside the open transaction (the transaction row, both
// postings, the audit row) has already executed by the time this fires,
// because RunInTx runs the whole unit of work and only calls tx.Commit once
// it has returned successfully. Disabling the proxy from inside this hook, and
// only then letting the "commit" statement actually go out, guarantees the
// connection is already severed before COMMIT can succeed, on every run.
type commitInterceptTracer struct {
	onCommit func()
	fired    atomic.Bool
}

func (tr *commitInterceptTracer) TraceQueryStart(
	ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData,
) context.Context {
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") && tr.fired.CompareAndSwap(false, true) {
		tr.onCommit()
	}
	return ctx
}

func (tr *commitInterceptTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

var _ pgx.QueryTracer = (*commitInterceptTracer)(nil)

// buildTracedPool opens a pool against dsn using tracer in place of the
// production otelpgx tracer. A single connection keeps which physical
// connection carries the eventual "commit" statement unambiguous.
func buildTracedPool(ctx context.Context, dsn string, tracer pgx.QueryTracer) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = tracer
	return pgxpool.NewWithConfig(ctx, cfg)
}

// TestChaosMidTransactionCutRollsBack is Case A from ADR-011: the connection
// to Postgres is severed after every statement of a Post has already run
// inside the open transaction, but before COMMIT can succeed. The append-only
// invariant requires that this leaves nothing behind: the post must fail, and
// neither the transaction row nor either posting may exist afterward.
func TestChaosMidTransactionCutRollsBack(t *testing.T) {
	ctx := context.Background()
	env := startChaosEnv(t)

	seedPool, err := postgres.NewPool(ctx, env.proxyDSN, 5)
	if err != nil {
		t.Fatalf("chaos test: open seed pool: %v", err)
	}
	seedRepo := postgres.NewRepository(seedPool)
	tenant := uuid.NewString()

	debit := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	credit := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := seedRepo.CreateAccount(ctx, tenant, debit); err != nil {
		t.Fatalf("chaos test: create debit account: %v", err)
	}
	if err := seedRepo.CreateAccount(ctx, tenant, credit); err != nil {
		t.Fatalf("chaos test: create credit account: %v", err)
	}
	// Close before wiring up the traced pool: the accounts are seeded over a
	// plain, healthy connection with nothing watching for "commit".
	seedPool.Close()

	tracer := &commitInterceptTracer{onCommit: func() {
		if err := env.proxy.Disable(); err != nil {
			t.Errorf("chaos test: disable proxy at commit: %v", err)
		}
	}}
	cutPool, err := buildTracedPool(ctx, env.proxyDSN, tracer)
	if err != nil {
		t.Fatalf("chaos test: open traced pool: %v", err)
	}
	defer cutPool.Close()

	cutRepo := postgres.NewRepository(cutPool)
	svc := ledger.NewTransactionService(cutRepo, discardLogger(), nil)
	txn := mkTxn(t, debit.ID, credit.ID)

	_, postErr := svc.Post(ctx, tenant, txn, nil)
	if postErr == nil {
		t.Fatal("expected Post to fail when the connection is cut before commit succeeds, got nil")
	}
	if !tracer.fired.Load() {
		t.Fatal("commit interceptor never fired; the test did not actually inject the failure at commit")
	}
	t.Logf("post failed as expected after the mid-transaction cut: %v", postErr)

	// Restore connectivity and reconnect fresh: a brand new pool, opened only
	// after the proxy is healthy again, so the verification query cannot be
	// affected by the connection that was just severed.
	if err := env.proxy.Enable(); err != nil {
		t.Fatalf("chaos test: re-enable proxy: %v", err)
	}
	verifyPool, err := postgres.NewPool(ctx, env.proxyDSN, 5)
	if err != nil {
		t.Fatalf("chaos test: open verify pool: %v", err)
	}
	defer verifyPool.Close()
	verifyRepo := postgres.NewRepository(verifyPool)

	if _, err := verifyRepo.GetTransaction(ctx, tenant, txn.ID); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Fatalf("expected a clean rollback: transaction %s should not exist, got %v", txn.ID, err)
	}

	txnID, err := uuid.Parse(txn.ID)
	if err != nil {
		t.Fatalf("chaos test: parse transaction id: %v", err)
	}
	var postingCount int
	if err := verifyPool.QueryRow(ctx,
		`select count(*) from postings where transaction_id = $1`, txnID,
	).Scan(&postingCount); err != nil {
		t.Fatalf("chaos test: count postings: %v", err)
	}
	if postingCount != 0 {
		t.Fatalf("postings for the rolled-back transaction %s = %d, want 0", txn.ID, postingCount)
	}
}

// TestChaosAmbiguousCommitReplayIsIdempotent is Case B from ADR-011: the
// ambiguous-commit scenario a client sees when it sends a post, the server
// commits, but the acknowledgement never reaches the client (a dropped
// connection, a timeout on the client side). The client cannot tell whether
// the post succeeded, so the natural, and correct, reaction is to retry with
// the same idempotency key. This must not double-post, and the
// idempotency-key row and the transaction commit must be consistent with each
// other (both exist, never one without the other), which is what makes the
// replay safe to trust.
func TestChaosAmbiguousCommitReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	env := startChaosEnv(t)

	pool, err := postgres.NewPool(ctx, env.proxyDSN, 5)
	if err != nil {
		t.Fatalf("chaos test: open pool: %v", err)
	}
	defer pool.Close()

	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	tenant := uuid.NewString()

	debit := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	credit := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, debit); err != nil {
		t.Fatalf("chaos test: create debit account: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, credit); err != nil {
		t.Fatalf("chaos test: create credit account: %v", err)
	}

	idem := &domain.Idempotency{Key: "ambiguous-commit-key"}
	txn := mkTxn(t, debit.ID, credit.ID)

	// The real post: it commits on a healthy connection. What is ambiguous in
	// the scenario this models is not whether the server committed, it did,
	// but whether the client's acknowledgement of that fact survived. That
	// ambiguity lives entirely on the client side, so the test proves the
	// server-side contract that makes a client-side retry safe: replaying the
	// same key must not create a second transaction.
	replayed, err := svc.Post(ctx, tenant, txn, idem)
	if err != nil {
		t.Fatalf("chaos test: first post: %v", err)
	}
	if replayed {
		t.Fatal("first post should not be reported as a replay")
	}

	// The client that never learned the outcome retries with the same key and
	// the same request body (so the fingerprint matches), on a healthy
	// connection, exactly as a real client would after a timeout.
	retryTxn := mkTxn(t, debit.ID, credit.ID)
	replayedRetry, err := svc.Post(ctx, tenant, retryTxn, idem)
	if err != nil {
		t.Fatalf("chaos test: replayed post: %v", err)
	}
	if !replayedRetry {
		t.Fatal("expected the retried post to report replayed=true")
	}
	if retryTxn.ID != txn.ID {
		t.Fatalf("replay returned a different transaction id: got %s, want %s", retryTxn.ID, txn.ID)
	}

	// Exactly one transaction exists for this key: no double-post.
	txnID, err := uuid.Parse(txn.ID)
	if err != nil {
		t.Fatalf("chaos test: parse transaction id: %v", err)
	}
	var txnCount int
	if err := pool.QueryRow(ctx,
		`select count(*) from transactions where id = $1`, txnID,
	).Scan(&txnCount); err != nil {
		t.Fatalf("chaos test: count transactions: %v", err)
	}
	if txnCount != 1 {
		t.Fatalf("transaction rows for %s = %d, want 1", txn.ID, txnCount)
	}

	// The atomicity guard: TransactionService.Post writes the transaction, the
	// idempotency key, and the audit row inside one RunInTx call, so they
	// commit together or not at all (see internal/postgres/repository.go,
	// Repository.RunInTx and TransactionService.Post). Confirm both sides are
	// present and agree, not just that a replay happened to work.
	rec, err := repo.GetIdempotencyKey(ctx, tenant, idem.Key)
	if err != nil {
		t.Fatalf("chaos test: get idempotency key: %v", err)
	}
	if rec.TransactionID != txn.ID {
		t.Fatalf("idempotency key points at transaction %s, want %s", rec.TransactionID, txn.ID)
	}
	if _, err := repo.GetTransaction(ctx, tenant, rec.TransactionID); err != nil {
		t.Fatalf("chaos test: transaction referenced by the idempotency key does not exist: %v", err)
	}
}
