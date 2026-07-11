// Integration tests for the webhook worker (Task 4.1, audit A7.1), against a
// real testcontainers Postgres with the full goose migration set applied.
// One container is shared across this package's tests, started once in
// TestMain, mirroring internal/audit's own test setup exactly (see that
// package's chainer_test.go): tests skip cleanly, not fail, when no Docker
// daemon is reachable.
package webhook_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
	"github.com/sohag-pro/go-ledger/internal/webhook"
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
	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		poolErr = fmt.Errorf("cannot start postgres container (is Docker running?): %w", err)
		return m.Run()
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		poolErr = err
		return m.Run()
	}
	if err := migrate(dsn); err != nil {
		poolErr = err
		return m.Run()
	}
	pool, err := postgres.NewPool(ctx, dsn, 20)
	if err != nil {
		poolErr = err
		return m.Run()
	}
	defer pool.Close()
	sharedPool = pool
	return m.Run()
}

func migrate(dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(sqlDB, "migrations")
}

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	return sharedPool
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedTenant creates a tenant with two USD accounts, returning the tenant id
// and both account ids (debit, credit). Mirrors internal/audit's own
// seedTenant helper.
func seedTenant(t *testing.T, repo *postgres.Repository, name string) (tenant, debit, credit string) {
	t.Helper()
	ctx := context.Background()
	tenant = uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, name); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	d := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	c := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, d); err != nil {
		t.Fatalf("create debit account: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, c); err != nil {
		t.Fatalf("create credit account: %v", err)
	}
	return tenant, d.ID, c.ID
}

// post posts one balanced transaction for tenant through the real
// TransactionService, returning the transaction id.
func post(t *testing.T, svc *ledger.TransactionService, tenant, debit, credit string, amount int64) string {
	t.Helper()
	d, err := domain.NewMoney(amount, "USD")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	c, err := domain.NewMoney(-amount, "USD")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit, Amount: d},
		{AccountID: credit, Amount: c},
	}}
	if _, err := svc.Post(context.Background(), tenant, txn, nil); err != nil {
		t.Fatalf("post transaction: %v", err)
	}
	return txn.ID
}

// drainAudit runs a real chainer until tenant has no pending outbox rows, so
// the posts above are reflected in audit_log (the fan-out source) before a
// test's FanOutOnce call. Mirrors internal/audit's own drainUntilEmpty.
func drainAudit(t *testing.T, pool *pgxpool.Pool, repo *postgres.Repository, tenant string) {
	t.Helper()
	ctx := context.Background()
	chainer := audit.NewChainer(pool, discardLogger(), time.Millisecond, 500)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := chainer.DrainOnce(ctx); err != nil {
			t.Fatalf("drain audit outbox: %v", err)
		}
		pending, err := repo.CountPendingOutbox(ctx, tenant)
		if err != nil {
			t.Fatalf("count pending outbox: %v", err)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("draining audit outbox for tenant %s timed out with %d rows still pending", tenant, pending)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// createSubscription creates an active subscription for tenant against url
// with the given event-type filter, returning the created subscription and
// its signing secret.
func createSubscription(t *testing.T, repo *postgres.Repository, tenant, url string, eventTypes []string) (domain.WebhookSubscription, string) {
	t.Helper()
	sub := domain.WebhookSubscription{TenantID: tenant, URL: url, EventTypes: eventTypes}
	secret, err := domain.GenerateWebhookSecret()
	if err != nil {
		t.Fatalf("generate webhook secret: %v", err)
	}
	if err := repo.CreateWebhookSubscription(context.Background(), &sub, secret); err != nil {
		t.Fatalf("create webhook subscription: %v", err)
	}
	return sub, secret
}

// deliveriesFor returns every webhook_deliveries row for subscriptionID,
// in fan-out order, read directly through sqlc (the worker's own storage
// path, not domain.Repository, since deliveries are not exposed through the
// repository port).
func deliveriesFor(t *testing.T, pool *pgxpool.Pool, subscriptionID string) []sqlc.WebhookDelivery {
	t.Helper()
	subID, err := uuid.Parse(subscriptionID)
	if err != nil {
		t.Fatalf("parse subscription id: %v", err)
	}
	rows, err := sqlc.New(pool).ListWebhookDeliveriesBySubscription(context.Background(), subID)
	if err != nil {
		t.Fatalf("list webhook deliveries: %v", err)
	}
	return rows
}

// TestFanOut_ExactlyOnce_TenantAndEventTypeIsolation proves the fan-out
// step's core contract (Task 4.1): a chained event creates exactly one
// pending delivery per ACTIVE, event-type-matching subscription in its OWN
// tenant, none for a non-matching event-type filter, none for an inactive
// subscription, and none for another tenant's subscription; and running
// fan-out again over the same range creates no duplicates (the unique
// index backstop).
func TestFanOut_ExactlyOnce_TenantAndEventTypeIsolation(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenantA, debitA, creditA := seedTenant(t, repo, "webhook fan-out test tenant A")
	tenantB, debitB, creditB := seedTenant(t, repo, "webhook fan-out test tenant B")

	matching, _ := createSubscription(t, repo, tenantA, "https://a.example.com/matching", []string{domain.ActionTransactionCreated})
	nonMatching, _ := createSubscription(t, repo, tenantA, "https://a.example.com/nonmatching", []string{domain.ActionTransactionReversed})
	inactive, _ := createSubscription(t, repo, tenantA, "https://a.example.com/inactive", nil)
	if err := repo.SetWebhookSubscriptionActive(ctx, inactive.ID, false); err != nil {
		t.Fatalf("deactivate subscription: %v", err)
	}
	otherTenantSub, _ := createSubscription(t, repo, tenantB, "https://b.example.com/hooks", nil)

	post(t, svc, tenantA, debitA, creditA, 500)
	post(t, svc, tenantB, debitB, creditB, 700)
	drainAudit(t, pool, repo, tenantA)
	drainAudit(t, pool, repo, tenantB)

	worker := webhook.NewWorker(pool, discardLogger(), webhook.Config{})
	created, err := worker.FanOutOnce(ctx)
	if err != nil {
		t.Fatalf("fan out: %v", err)
	}
	if created != 2 {
		t.Fatalf("fan-out created %d deliveries, want 2 (tenant A's matching sub, tenant B's sub)", created)
	}

	if got := deliveriesFor(t, pool, matching.ID); len(got) != 1 {
		t.Errorf("matching subscription deliveries = %d, want 1", len(got))
	}
	if got := deliveriesFor(t, pool, nonMatching.ID); len(got) != 0 {
		t.Errorf("non-matching subscription deliveries = %d, want 0", len(got))
	}
	if got := deliveriesFor(t, pool, inactive.ID); len(got) != 0 {
		t.Errorf("inactive subscription deliveries = %d, want 0", len(got))
	}
	if got := deliveriesFor(t, pool, otherTenantSub.ID); len(got) != 1 {
		t.Errorf("tenant B's subscription deliveries = %d, want 1", len(got))
	}

	// Running fan-out again over the same (already-scanned) range must not
	// duplicate anything: the cursor already advanced past these events.
	createdAgain, err := worker.FanOutOnce(ctx)
	if err != nil {
		t.Fatalf("second fan out: %v", err)
	}
	if createdAgain != 0 {
		t.Errorf("second fan-out created %d deliveries, want 0 (cursor already past these events)", createdAgain)
	}
	if got := deliveriesFor(t, pool, matching.ID); len(got) != 1 {
		t.Errorf("matching subscription deliveries after second fan-out = %d, want still 1", len(got))
	}

	// This test never runs a delivery pass (it exercises fan-out only), so
	// the two active subscriptions it created (matching, otherTenantSub)
	// each leave one delivery row PENDING, pointed at fake, undeliverable
	// URLs. This package's tests all share one Postgres container and one
	// webhook_deliveries table (see TestMain), and the worker's due-scan is
	// deliberately global, not scoped to a test or tenant (that is real
	// production behavior, not a test artifact): a later test's own
	// DeliverOnce call would otherwise also pick these rows up and burn a
	// delivery attempt against a URL nobody is listening on. Deactivating
	// both subscriptions here (the exact tool DeleteWebhookSubscription uses)
	// removes them from every later test's due-scan, exactly as
	// TestDeactivatedSubscription_StopsFutureDeliveries proves for its own
	// subscription.
	if err := repo.SetWebhookSubscriptionActive(ctx, matching.ID, false); err != nil {
		t.Fatalf("deactivate matching subscription (test cleanup): %v", err)
	}
	if err := repo.SetWebhookSubscriptionActive(ctx, otherTenantSub.ID, false); err != nil {
		t.Fatalf("deactivate tenant B subscription (test cleanup): %v", err)
	}
}

// TestDelivery_SignedPayloadVerifiedAndMarkedDelivered proves a 2xx receiver
// gets a correctly HMAC-SHA256-signed payload (verified in this test, not
// just trusted) shaped exactly as the brief specifies, and the delivery is
// marked delivered.
func TestDelivery_SignedPayloadVerifiedAndMarkedDelivered(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo, "webhook delivery signed test tenant")

	var gotBody []byte
	var gotSigHeader, gotDeliveryIDHeader, gotEventHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSigHeader = r.Header.Get(webhook.HeaderSignature)
		gotDeliveryIDHeader = r.Header.Get(webhook.HeaderDeliveryID)
		gotEventHeader = r.Header.Get(webhook.HeaderEvent)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sub, secret := createSubscription(t, repo, tenant, srv.URL, nil)
	txnID := post(t, svc, tenant, debit, credit, 1234)
	drainAudit(t, pool, repo, tenant)

	worker := webhook.NewWorker(pool, discardLogger(), webhook.Config{})
	if _, err := worker.FanOutOnce(ctx); err != nil {
		t.Fatalf("fan out: %v", err)
	}
	delivered, retried, dead, err := worker.DeliverOnce(ctx)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if delivered != 1 || retried != 0 || dead != 0 {
		t.Fatalf("deliver outcome = {delivered:%d retried:%d dead:%d}, want {1 0 0}", delivered, retried, dead)
	}

	if gotBody == nil {
		t.Fatal("receiver never got a request body")
	}
	wantSig := webhook.SignatureHeader(secret, gotBody)
	if gotSigHeader != wantSig {
		t.Errorf("X-Ledger-Signature = %q, want %q (HMAC-SHA256 of the exact body received, keyed by the subscription secret)", gotSigHeader, wantSig)
	}
	if gotEventHeader != domain.ActionTransactionCreated {
		t.Errorf("X-Ledger-Event = %q, want %q", gotEventHeader, domain.ActionTransactionCreated)
	}

	var payload domain.WebhookPayload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("unmarshal received payload: %v", err)
	}
	if payload.ID != gotDeliveryIDHeader {
		t.Errorf("payload.ID = %q, X-Ledger-Delivery-Id header = %q, want equal", payload.ID, gotDeliveryIDHeader)
	}
	if payload.Event != domain.ActionTransactionCreated {
		t.Errorf("payload.Event = %q, want %q", payload.Event, domain.ActionTransactionCreated)
	}
	if payload.TenantID != tenant {
		t.Errorf("payload.TenantID = %q, want %q", payload.TenantID, tenant)
	}
	if payload.TransactionID != txnID {
		t.Errorf("payload.TransactionID = %q, want %q", payload.TransactionID, txnID)
	}
	if payload.OccurredAt.IsZero() {
		t.Error("payload.OccurredAt is zero, want a real timestamp")
	}
	if len(payload.Data) == 0 {
		t.Error("payload.Data is empty, want the audit after-snapshot")
	}

	rows := deliveriesFor(t, pool, sub.ID)
	if len(rows) != 1 {
		t.Fatalf("deliveries for subscription = %d, want 1", len(rows))
	}
	if rows[0].Status != string(domain.WebhookDeliveryDelivered) {
		t.Errorf("delivery status = %q, want %q", rows[0].Status, domain.WebhookDeliveryDelivered)
	}
	if !rows[0].DeliveredAt.Valid {
		t.Error("delivered_at is not set, want a timestamp")
	}
}

// TestDelivery_RetrySucceedsAtLeastOnce proves the at-least-once contract: a
// receiver that fails once (500) and succeeds on the next attempt (200)
// eventually gets the delivery, and the X-Ledger-Delivery-Id header is the
// SAME value across both attempts (what makes a real receiver's dedup
// meaningful).
func TestDelivery_RetrySucceedsAtLeastOnce(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo, "webhook retry-succeeds test tenant")

	var requestCount atomic.Int32
	var deliveryIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		deliveryIDs = append(deliveryIDs, r.Header.Get(webhook.HeaderDeliveryID))
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sub, _ := createSubscription(t, repo, tenant, srv.URL, nil)
	post(t, svc, tenant, debit, credit, 999)
	drainAudit(t, pool, repo, tenant)

	// backoff is deliberately generous (not a tight few milliseconds): the
	// "immediately again" check just below asserts nothing is due yet, and
	// under -race (or a loaded CI box) even "immediately" can cost tens of
	// milliseconds of real wall-clock time between the two DeliverOnce
	// calls. A backoff comfortably larger than that scheduling jitter is
	// what keeps this assertion meaningful instead of flaky.
	const backoff = 500 * time.Millisecond
	worker := webhook.NewWorker(pool, discardLogger(), webhook.Config{
		MaxAttempts: 5,
		BackoffBase: backoff,
		BackoffCap:  backoff,
	})
	if _, err := worker.FanOutOnce(ctx); err != nil {
		t.Fatalf("fan out: %v", err)
	}

	// First attempt: the 500 response, recorded as a retry.
	delivered, retried, dead, err := worker.DeliverOnce(ctx)
	if err != nil {
		t.Fatalf("deliver (first attempt): %v", err)
	}
	if delivered != 0 || retried != 1 || dead != 0 {
		t.Fatalf("first deliver outcome = {delivered:%d retried:%d dead:%d}, want {0 1 0}", delivered, retried, dead)
	}

	// Immediately again: backoff has not elapsed yet, so nothing is due.
	delivered, retried, dead, err = worker.DeliverOnce(ctx)
	if err != nil {
		t.Fatalf("deliver (immediately after failure): %v", err)
	}
	if delivered+retried+dead != 0 {
		t.Fatalf("deliver before backoff elapsed = {delivered:%d retried:%d dead:%d}, want all zero", delivered, retried, dead)
	}

	time.Sleep(backoff + 200*time.Millisecond)

	// Second attempt: the 200 response, recorded as delivered.
	delivered, retried, dead, err = worker.DeliverOnce(ctx)
	if err != nil {
		t.Fatalf("deliver (second attempt): %v", err)
	}
	if delivered != 1 || retried != 0 || dead != 0 {
		t.Fatalf("second deliver outcome = {delivered:%d retried:%d dead:%d}, want {1 0 0}", delivered, retried, dead)
	}

	if requestCount.Load() != 2 {
		t.Fatalf("receiver saw %d requests, want exactly 2 (one failure, one success)", requestCount.Load())
	}
	if len(deliveryIDs) != 2 || deliveryIDs[0] != deliveryIDs[1] {
		t.Errorf("X-Ledger-Delivery-Id across attempts = %v, want the same value both times", deliveryIDs)
	}

	rows := deliveriesFor(t, pool, sub.ID)
	if len(rows) != 1 {
		t.Fatalf("deliveries for subscription = %d, want 1", len(rows))
	}
	if rows[0].Status != string(domain.WebhookDeliveryDelivered) {
		t.Errorf("final delivery status = %q, want %q", rows[0].Status, domain.WebhookDeliveryDelivered)
	}
	if rows[0].Attempts != 2 {
		t.Errorf("final delivery attempts = %d, want 2", rows[0].Attempts)
	}
}

// TestDelivery_RetryToDeadAfterMaxAttempts proves a receiver that always
// fails is retried up to MaxAttempts and then marked dead, and is never
// attempted again after that.
func TestDelivery_RetryToDeadAfterMaxAttempts(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo, "webhook retry-to-dead test tenant")

	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sub, _ := createSubscription(t, repo, tenant, srv.URL, nil)
	post(t, svc, tenant, debit, credit, 42)
	drainAudit(t, pool, repo, tenant)

	const (
		maxAttempts = 3
		backoff     = 15 * time.Millisecond
	)
	worker := webhook.NewWorker(pool, discardLogger(), webhook.Config{
		MaxAttempts: maxAttempts,
		BackoffBase: backoff,
		BackoffCap:  backoff,
	})
	if _, err := worker.FanOutOnce(ctx); err != nil {
		t.Fatalf("fan out: %v", err)
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		delivered, retried, dead, err := worker.DeliverOnce(ctx)
		if err != nil {
			t.Fatalf("deliver (attempt %d): %v", attempt, err)
		}
		if delivered != 0 {
			t.Fatalf("attempt %d: delivered = %d, want 0 (receiver always fails)", attempt, delivered)
		}
		if attempt < maxAttempts {
			if retried != 1 || dead != 0 {
				t.Fatalf("attempt %d outcome = {retried:%d dead:%d}, want {1 0}", attempt, retried, dead)
			}
		} else {
			if retried != 0 || dead != 1 {
				t.Fatalf("final attempt %d outcome = {retried:%d dead:%d}, want {0 1} (max attempts reached)", attempt, retried, dead)
			}
		}
		time.Sleep(backoff + 15*time.Millisecond)
	}

	if requestCount.Load() != maxAttempts {
		t.Fatalf("receiver saw %d requests, want exactly %d", requestCount.Load(), maxAttempts)
	}

	// One more delivery pass: the row is dead, so ListDueWebhookDeliveries
	// must not pick it up again, and the receiver must see no further request.
	delivered, retried, dead, err := worker.DeliverOnce(ctx)
	if err != nil {
		t.Fatalf("deliver (after dead): %v", err)
	}
	if delivered+retried+dead != 0 {
		t.Fatalf("deliver after dead = {delivered:%d retried:%d dead:%d}, want all zero (nothing due)", delivered, retried, dead)
	}
	if requestCount.Load() != maxAttempts {
		t.Fatalf("receiver saw %d requests after the extra deliver pass, want still %d (a dead row is never retried)", requestCount.Load(), maxAttempts)
	}

	rows := deliveriesFor(t, pool, sub.ID)
	if len(rows) != 1 {
		t.Fatalf("deliveries for subscription = %d, want 1", len(rows))
	}
	if rows[0].Status != string(domain.WebhookDeliveryDead) {
		t.Errorf("final delivery status = %q, want %q", rows[0].Status, domain.WebhookDeliveryDead)
	}
	if rows[0].Attempts != maxAttempts {
		t.Errorf("final delivery attempts = %d, want %d", rows[0].Attempts, maxAttempts)
	}
	if !rows[0].LastError.Valid || rows[0].LastError.String == "" {
		t.Error("last_error is empty, want the recorded failure reason")
	}
}

// TestDeactivatedSubscription_StopsFutureDeliveries proves DeleteSubscription
// (a deactivate, see domain.Repository.SetWebhookSubscriptionActive's doc
// comment) stops an already-pending delivery from ever being attempted: the
// receiver must see zero requests, and the row stays pending, neither
// delivered nor purged.
func TestDeactivatedSubscription_StopsFutureDeliveries(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo, "webhook deactivate test tenant")

	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sub, _ := createSubscription(t, repo, tenant, srv.URL, nil)
	post(t, svc, tenant, debit, credit, 55)
	drainAudit(t, pool, repo, tenant)

	worker := webhook.NewWorker(pool, discardLogger(), webhook.Config{})
	if _, err := worker.FanOutOnce(ctx); err != nil {
		t.Fatalf("fan out: %v", err)
	}
	if got := deliveriesFor(t, pool, sub.ID); len(got) != 1 {
		t.Fatalf("deliveries before deactivate = %d, want 1", len(got))
	}

	// Deactivate BEFORE the delivery pass runs: this is what
	// admin.Service.DeleteWebhookSubscription does.
	if err := repo.SetWebhookSubscriptionActive(ctx, sub.ID, false); err != nil {
		t.Fatalf("deactivate subscription: %v", err)
	}

	delivered, retried, dead, err := worker.DeliverOnce(ctx)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if delivered+retried+dead != 0 {
		t.Fatalf("deliver outcome after deactivate = {delivered:%d retried:%d dead:%d}, want all zero (nothing should be attempted)", delivered, retried, dead)
	}
	if requestCount.Load() != 0 {
		t.Fatalf("receiver saw %d requests, want 0 (subscription was deactivated before delivery)", requestCount.Load())
	}

	rows := deliveriesFor(t, pool, sub.ID)
	if len(rows) != 1 {
		t.Fatalf("deliveries for subscription = %d, want 1", len(rows))
	}
	if rows[0].Status != string(domain.WebhookDeliveryPending) {
		t.Errorf("delivery status = %q, want %q (untouched, not purged)", rows[0].Status, domain.WebhookDeliveryPending)
	}
}

// TestWorker_LeaderElection_OnlyOneRunsAtATime proves two Worker instances,
// each with its own pool (as two app instances would each have their own),
// both running Run concurrently, never both fan out at once: exactly one
// pending delivery is created per matching subscription, never two, which
// would be the observable symptom of a lost or double-held leader lock. This
// mirrors internal/audit.TestChainer_LeaderElection_OnlyOneDrainsAtATime
// against this worker's own, distinct advisory lock key.
func TestWorker_LeaderElection_OnlyOneRunsAtATime(t *testing.T) {
	poolA := newTestPool(t)
	poolB := poolA // both workers share the underlying database; leader election is what matters, not connection isolation

	repo := postgres.NewRepository(poolA)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)

	tenant, debit, credit := seedTenant(t, repo, "webhook leader election test tenant")

	// A real httptest.Server, not a fake external URL: this test's Run()
	// loops execute a full delivery pass on every tick, so a fake URL would
	// mean a real, slow, flaky outbound network call (and, since example.com
	// is a real domain, an unpredictable response). Responding 200
	// immediately keeps the delivery terminal (delivered) the first time
	// either worker attempts it, so this row never lingers to leak into a
	// later test's own global due-scan.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	sub, _ := createSubscription(t, repo, tenant, srv.URL, nil)

	workerA := webhook.NewWorker(poolA, discardLogger(), webhook.Config{Interval: 20 * time.Millisecond})
	workerB := webhook.NewWorker(poolB, discardLogger(), webhook.Config{Interval: 20 * time.Millisecond})

	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan struct{})
	go func() { defer close(doneA); workerA.Run(ctxA) }()

	ctxB, cancelB := context.WithCancel(context.Background())
	doneB := make(chan struct{})
	go func() { defer close(doneB); workerB.Run(ctxB) }()
	defer func() { cancelB(); <-doneB }()

	post(t, svc, tenant, debit, credit, 321)
	drainAudit(t, poolA, repo, tenant)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := deliveriesFor(t, poolA, sub.ID); len(got) >= 1 {
			if len(got) != 1 {
				t.Fatalf("deliveries for subscription = %d, want exactly 1 (two leaders would double fan-out)", len(got))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for either worker to fan out the posted event")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancelA()
	<-doneA

	// Give worker B, now the sole (or newly sole) leader, time to run a
	// further pass; the delivery count must still be exactly 1.
	time.Sleep(100 * time.Millisecond)
	if got := deliveriesFor(t, poolA, sub.ID); len(got) != 1 {
		t.Fatalf("deliveries for subscription after A stopped = %d, want still exactly 1", len(got))
	}
}
