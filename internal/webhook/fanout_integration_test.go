// Integration test for Task 10 (ADR-025): a chained non-transaction
// lifecycle event (for example approval.requested) must fan out into a
// webhook delivery whose payload carries subject_type/subject_id and an
// empty (omitted) transaction_id, not a panic or a silently wrong
// nil-UUID string from calling pgtype.UUID.String() without a .Valid
// guard (the landmine Task 3's report flagged). This file shares
// TestMain, newTestPool, seedTenant, drainAudit, createSubscription, and
// deliveriesFor with worker_test.go: same package, same shared container.
package webhook_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/webhook"
)

// TestFanOut_LifecycleEvent_CarriesSubjectNoTransactionID proves the
// subject-based webhook contract Task 10 adds: a chained
// "approval.requested" lifecycle row (null transaction_id, subject_type
// "pending_transaction", a real subject_id, hash_version 2) delivers a
// payload with the event name, an empty/absent transaction_id, and the
// matching subject_type/subject_id, to a subscription filtered on that
// exact event type.
func TestFanOut_LifecycleEvent_CarriesSubjectNoTransactionID(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	tenant, _, _ := seedTenant(t, repo, "webhook lifecycle event test tenant")

	sub, _ := createSubscription(t, repo, tenant, "https://example.com/lifecycle", []string{"approval.requested"})

	subjectID := uuid.NewString()
	after, err := json.Marshal(map[string]any{"pending_id": subjectID})
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	err = repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.AppendAuditOutbox(ctx, tenant, domain.AuditEvent{
			Action:      "approval.requested",
			Actor:       tenant,
			After:       after,
			SubjectType: "pending_transaction",
			SubjectID:   subjectID,
			HashVersion: domain.AuditHashV2,
		})
	})
	if err != nil {
		t.Fatalf("append lifecycle audit outbox: %v", err)
	}

	drainAudit(t, pool, repo, tenant)

	worker := webhook.NewWorker(pool, discardLogger(), webhook.Config{})
	created, err := worker.FanOutOnce(ctx)
	if err != nil {
		t.Fatalf("fan out: %v", err)
	}
	if created != 1 {
		t.Fatalf("fan-out created %d deliveries, want 1 (the matching subscription)", created)
	}

	rows := deliveriesFor(t, pool, sub.ID)
	if len(rows) != 1 {
		t.Fatalf("deliveries for subscription = %d, want 1", len(rows))
	}

	var payload domain.WebhookPayload
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Event != "approval.requested" {
		t.Errorf("payload.Event = %q, want %q", payload.Event, "approval.requested")
	}
	if payload.TransactionID != "" {
		t.Errorf("payload.TransactionID = %q, want empty (a lifecycle event has no transaction)", payload.TransactionID)
	}
	if payload.SubjectType != "pending_transaction" {
		t.Errorf("payload.SubjectType = %q, want %q", payload.SubjectType, "pending_transaction")
	}
	if payload.SubjectID != subjectID {
		t.Errorf("payload.SubjectID = %q, want %q", payload.SubjectID, subjectID)
	}

	// TransactionID must be omitempty on WebhookPayload: a consumer should
	// never see a misleading present-but-empty transaction_id key on a
	// lifecycle event's delivered JSON.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rows[0].Payload, &raw); err != nil {
		t.Fatalf("unmarshal raw payload: %v", err)
	}
	if _, ok := raw["transaction_id"]; ok {
		t.Error("delivered payload JSON contains a transaction_id key, want it omitted for a lifecycle event")
	}

	// This test never runs a delivery pass (it exercises fan-out only), so
	// the subscription created above leaves one delivery row PENDING,
	// pointed at a fake, undeliverable URL. This package's tests all share
	// one Postgres container and one webhook_deliveries table (see
	// TestMain in worker_test.go), and the worker's due-scan is
	// deliberately global, not scoped to a test or tenant: a later test's
	// own DeliverOnce call would otherwise also pick this row up and burn a
	// delivery attempt against a URL nobody is listening on (exactly the
	// failure mode TestFanOut_ExactlyOnce_TenantAndEventTypeIsolation's own
	// cleanup comment describes). Deactivating removes it from every later
	// test's due-scan.
	if err := repo.SetWebhookSubscriptionActive(ctx, sub.ID, false); err != nil {
		t.Fatalf("deactivate subscription (test cleanup): %v", err)
	}
}
