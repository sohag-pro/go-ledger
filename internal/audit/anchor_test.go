package audit_test

// Integration tests for the audit anchor job (Task 5.3, audit A2.4). See
// migration 0025's own doc comment for the motivation: a hash chain alone
// proves only internal self-consistency, and a privileged rewrite that
// recomputes every downstream hash consistently leaves even a full
// from-genesis Verify with nothing to find. These tests prove the anchor job
// actually records + logs the head (the mechanism), and then prove the
// motivating claim itself: a self-consistent forgery of the tenant's last
// row escapes AuditService.Verify entirely, yet diverges from the anchor
// recorded before the forgery, which is exactly what makes the anchor,
// logged off-box, worth having.
//
// None of these tests call t.Parallel(): AnchorOnce reads and anchors EVERY
// tenant currently in the shared container (ListAuditHeads has no tenant
// filter, by design, since the job is a cross-tenant background worker), so
// running these concurrently with each other could see one test's AnchorOnce
// call anchor a tenant a sibling test has not finished setting up yet.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// jsonLogger returns a slog.Logger writing structured JSON into w, so a test
// can parse and inspect the exact lines the anchor job emitted.
func jsonLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// syncBuffer is a concurrency-safe bytes.Buffer. Needed only by
// TestAnchorJob_LeaderElection_OnlyOneLeadsAtATime: two AnchorJob.Run
// goroutines write log lines into it concurrently with the test goroutine's
// own polling reads, which a plain bytes.Buffer does not tolerate (every
// other test in this file logs synchronously, via AnchorOnce, with no
// concurrent writer, so a plain bytes.Buffer is fine there).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Contains(substr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Contains(b.buf.Bytes(), []byte(substr))
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// findAnchorLogLine scans buf's JSON lines for one with msg "audit anchor"
// and the given tenant_id, returning its chain_seq and row_hash fields (and
// ok=true), or ok=false if no such line exists.
func findAnchorLogLine(t *testing.T, buf *bytes.Buffer, tenantID string) (chainSeq int64, rowHash string, ok bool) {
	t.Helper()
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry["msg"] != "audit anchor" {
			continue
		}
		if entry["tenant_id"] != tenantID {
			continue
		}
		seq, _ := entry["chain_seq"].(float64)
		hash, _ := entry["row_hash"].(string)
		return int64(seq), hash, true
	}
	return 0, "", false
}

// TestAnchorJob_RecordsHeadAndLogs proves the mechanism: after posting and
// draining a real chain, running the anchor job once inserts an
// audit_anchors row matching the tenant's live head, AND emits a structured
// "audit anchor" log line carrying the same tenant_id, chain_seq, and
// row_hash (the off-box copy Task 5.6's log shipper is meant to capture).
func TestAnchorJob_RecordsHeadAndLogs(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo)
	const n = 3
	for i := 0; i < n; i++ {
		post(t, svc, tenant, debit, credit, int64(100+i))
	}
	chainer := audit.NewChainer(pool, discardLogger(), time.Millisecond, 500)
	drainUntilEmpty(t, chainer, repo, tenant)

	wantSeq, wantHash, ok, err := repo.GetAuditHead(ctx, tenant)
	if err != nil {
		t.Fatalf("get audit head: %v", err)
	}
	if !ok {
		t.Fatalf("get audit head: no head, want one after posting %d rows", n)
	}

	var buf bytes.Buffer
	anchorJob := audit.NewAnchorJob(pool, jsonLogger(&buf), time.Hour)
	if _, err := anchorJob.AnchorOnce(ctx); err != nil {
		t.Fatalf("anchor once: %v", err)
	}

	anchor, ok, err := repo.LatestAuditAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("latest audit anchor: %v", err)
	}
	if !ok {
		t.Fatal("latest audit anchor: no anchor recorded, want one after AnchorOnce")
	}
	if anchor.ChainSeq != wantSeq || anchor.RowHash != wantHash {
		t.Errorf("anchor = {chain_seq:%d row_hash:%q}, want {chain_seq:%d row_hash:%q} (the tenant's live head at anchor time)",
			anchor.ChainSeq, anchor.RowHash, wantSeq, wantHash)
	}

	logSeq, logHash, found := findAnchorLogLine(t, &buf, tenant)
	if !found {
		t.Fatalf(`no "audit anchor" log line found for tenant %s; log output:\n%s`, tenant, buf.String())
	}
	if logSeq != wantSeq || logHash != wantHash {
		t.Errorf("logged anchor = {chain_seq:%d row_hash:%q}, want {chain_seq:%d row_hash:%q}", logSeq, logHash, wantSeq, wantHash)
	}
}

// TestAnchorJob_NoAuditRows_AnchorsNothing proves a brand-new tenant with no
// audit_log rows at all is simply absent from ListAuditHeads: AnchorOnce
// neither anchors nor logs anything for it, rather than erroring or writing
// a nonsensical zero-value anchor.
func TestAnchorJob_NoAuditRows_AnchorsNothing(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant, _, _ := seedTenant(t, repo)

	var buf bytes.Buffer
	anchorJob := audit.NewAnchorJob(pool, jsonLogger(&buf), time.Hour)
	if _, err := anchorJob.AnchorOnce(ctx); err != nil {
		t.Fatalf("anchor once: %v", err)
	}

	if _, ok, err := repo.LatestAuditAnchor(ctx, tenant); err != nil {
		t.Fatalf("latest audit anchor: %v", err)
	} else if ok {
		t.Error("latest audit anchor: got one for a tenant with no audit rows, want none")
	}
	if _, _, found := findAnchorLogLine(t, &buf, tenant); found {
		t.Error(`found an "audit anchor" log line for a tenant with no audit rows, want none`)
	}
}

// TestAnchorJob_SuffixRewriteDivergesFromAnchorButVerifyStillPasses is the
// acceptance test for WHY the anchor job exists (migration 0025's own doc
// comment): it forges the tenant's last audit row in a way that is fully
// self-consistent (new content, a freshly and correctly recomputed
// row_hash, the same prev_hash as before, since there is no downstream row
// whose prev_hash would also need to change). AuditService.Verify, which
// only ever recomputes and compares against what is currently stored, must
// still report the chain valid: this is not a bug in Verify, it is the
// fundamental limit a hash chain alone has against a privileged rewriter.
// The anchor recorded BEFORE the forgery is what catches it: the live head's
// row_hash the forged row now carries no longer matches what was anchored
// for that same chain_seq.
func TestAnchorJob_SuffixRewriteDivergesFromAnchorButVerifyStillPasses(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	auditSvc := ledger.NewAuditService(repo)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo)
	const n = 3
	for i := 0; i < n; i++ {
		post(t, svc, tenant, debit, credit, int64(500+i))
	}
	chainer := audit.NewChainer(pool, discardLogger(), time.Millisecond, 500)
	drainUntilEmpty(t, chainer, repo, tenant)

	anchorJob := audit.NewAnchorJob(pool, discardLogger(), time.Hour)
	if _, err := anchorJob.AnchorOnce(ctx); err != nil {
		t.Fatalf("anchor once: %v", err)
	}
	anchorBefore, ok, err := repo.LatestAuditAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("latest audit anchor: %v", err)
	}
	if !ok {
		t.Fatal("latest audit anchor: no anchor recorded")
	}

	// Sanity: the chain verifies clean before any forgery.
	before, err := auditSvc.Verify(ctx, tenant)
	if err != nil || !before.Valid {
		t.Fatalf("verify before forgery: result=%+v err=%v, want a valid chain", before, err)
	}

	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("audit rows = %d, want %d", len(rows), n)
	}
	last := rows[len(rows)-1]
	if last.RowHash != anchorBefore.RowHash {
		t.Fatalf("test setup bug: last row's hash %q != anchored hash %q", last.RowHash, anchorBefore.RowHash)
	}

	// Forge the last row: new After content, correctly recomputed RowHash
	// (same PrevHash: it is the chain's tail, so nothing downstream needs to
	// agree with it). This is what makes the forgery self-consistent.
	forged := last
	forged.After = []byte(`{"forged":true}`)
	forgedHash := domain.ComputeAuditRowHash(tenant, forged, forged.PrevHash)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin forge tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL audit.allow_purge = 'on'`); err != nil {
		t.Fatalf("set local: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_log SET after = $1, row_hash = $2 WHERE id = $3`,
		forged.After, forgedHash, uuid.MustParse(last.ID),
	); err != nil {
		t.Fatalf("forge update: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit forge: %v", err)
	}

	// The crux: a full from-genesis Verify still reports the chain valid.
	// This is not a false negative to fix; it is the exact limitation
	// anchoring exists to cover (see this test's own doc comment).
	after, err := auditSvc.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify after forgery: %v", err)
	}
	if !after.Valid {
		t.Fatalf("verify after a self-consistent forgery reported invalid (checked=%d break=%s): "+
			"this test's premise is that a correctly-recomputed forgery of the tail is NOT detectable by Verify alone",
			after.Checked, after.FirstBreakID)
	}

	// The anchor recorded before the forgery is what catches it: the live
	// head's row_hash no longer matches what was anchored for the same
	// chain_seq.
	liveSeq, liveHash, ok, err := repo.GetAuditHead(ctx, tenant)
	if err != nil {
		t.Fatalf("get audit head after forgery: %v", err)
	}
	if !ok {
		t.Fatal("get audit head after forgery: no head")
	}
	if liveSeq != anchorBefore.ChainSeq {
		t.Fatalf("live head chain_seq = %d, want it unchanged at %d (the forgery rewrote content in place, not the row count)",
			liveSeq, anchorBefore.ChainSeq)
	}
	if liveHash == anchorBefore.RowHash {
		t.Fatal("live head row_hash still matches the pre-forgery anchor, want it to have changed (the forgery updated row_hash too)")
	}
	if liveHash != forgedHash {
		t.Errorf("live head row_hash = %q, want the freshly forged %q", liveHash, forgedHash)
	}
}

// TestAnchorJob_LeaderElection_OnlyOneLeadsAtATime proves the job's leader
// election actually works, on its own DISTINCT advisory lock key (Task 5.3):
// two AnchorJob instances run concurrently via Run against two independent
// pools (standing in for two app instances); only one ever logs "acquired
// leadership", and after that one's context is cancelled, the other takes
// over and itself logs "acquired leadership". It uses its own dedicated
// pools (not the package's shared one) precisely so it is free to run two
// long-lived Run loops without racing every other test's use of the shared
// container's tenants.
func TestAnchorJob_LeaderElection_OnlyOneLeadsAtATime(t *testing.T) {
	poolA := newPool(t, 5)
	poolB := newPool(t, 5)

	var bufA, bufB syncBuffer
	jobA := audit.NewAnchorJob(poolA, jsonLogger(&bufA), 20*time.Millisecond)
	jobB := audit.NewAnchorJob(poolB, jsonLogger(&bufB), 20*time.Millisecond)

	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan struct{})
	go func() { defer close(doneA); jobA.Run(ctxA) }()
	t.Cleanup(func() { cancelA(); <-doneA })

	ctxB, cancelB := context.WithCancel(context.Background())
	doneB := make(chan struct{})
	go func() { defer close(doneB); jobB.Run(ctxB) }()
	t.Cleanup(func() { cancelB(); <-doneB })

	deadline := time.Now().Add(5 * time.Second)
	for !bufA.Contains("acquired leadership") && !bufB.Contains("acquired leadership") {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for either instance to acquire leadership\nA:\n%s\nB:\n%s", bufA.String(), bufB.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	aLed := bufA.Contains("acquired leadership")
	bLed := bufB.Contains("acquired leadership")
	if aLed == bLed {
		t.Fatalf("exactly one instance must have led so far, got A led=%v B led=%v", aLed, bLed)
	}

	// Stop whichever instance led, and wait for its goroutine to fully
	// return (guaranteeing its advisory lock is released), then confirm the
	// OTHER instance takes over. The final t.Cleanup calls above still fire
	// at test end and are idempotent against whichever of cancelA/cancelB
	// already ran here.
	var follower *syncBuffer
	if aLed {
		cancelA()
		<-doneA
		follower = &bufB
	} else {
		cancelB()
		<-doneB
		follower = &bufA
	}
	deadline = time.Now().Add(5 * time.Second)
	for !follower.Contains("acquired leadership") {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the surviving instance to take over leadership:\n%s", follower.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
