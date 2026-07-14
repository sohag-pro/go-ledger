package domain

import (
	"testing"
	"time"
)

func baseAuditEntry() AuditEntry {
	return AuditEntry{
		Action:        ActionTransactionCreated,
		TransactionID: "txn-1",
		Actor:         "tenant-1",
		Before:        nil,
		After:         []byte(`{"id":"txn-1"}`),
		CreatedAt:     time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}
}

func TestComputeAuditRowHashStable(t *testing.T) {
	e := baseAuditEntry()
	h1 := ComputeAuditRowHash("tenant-1", e, AuditGenesisHash)
	h2 := ComputeAuditRowHash("tenant-1", e, AuditGenesisHash)
	if h1 != h2 {
		t.Fatalf("same entry and prevHash produced different hashes: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(h1))
	}
}

func TestComputeAuditRowHashGenesisIsEmptyString(t *testing.T) {
	if AuditGenesisHash != "" {
		t.Errorf("AuditGenesisHash = %q, want empty string", AuditGenesisHash)
	}
}

// TestComputeAuditRowHashEveryFieldMoves proves every hashed field, including
// prevHash itself, is actually load-bearing: changing any one of them alone
// must move the hash. This is what makes the chain tamper-evident: an
// attacker cannot alter one field of a stored row and still match its
// row_hash.
func TestComputeAuditRowHashEveryFieldMoves(t *testing.T) {
	base := baseAuditEntry()
	const baseTenant = "tenant-1"
	baseHash := ComputeAuditRowHash(baseTenant, base, AuditGenesisHash)

	cases := map[string]struct {
		tenantID string
		entry    AuditEntry
		prevHash string
	}{
		"tenant id":      {tenantID: "tenant-2", entry: base, prevHash: AuditGenesisHash},
		"action":         {tenantID: baseTenant, entry: func() AuditEntry { e := base; e.Action = "transaction.other"; return e }(), prevHash: AuditGenesisHash},
		"transaction id": {tenantID: baseTenant, entry: func() AuditEntry { e := base; e.TransactionID = "txn-2"; return e }(), prevHash: AuditGenesisHash},
		"actor":          {tenantID: baseTenant, entry: func() AuditEntry { e := base; e.Actor = "tenant-2"; return e }(), prevHash: AuditGenesisHash},
		"before": {tenantID: baseTenant, entry: func() AuditEntry {
			e := base
			e.Before = []byte(`{"was":"something"}`)
			return e
		}(), prevHash: AuditGenesisHash},
		"after": {tenantID: baseTenant, entry: func() AuditEntry {
			e := base
			e.After = []byte(`{"id":"txn-1","extra":true}`)
			return e
		}(), prevHash: AuditGenesisHash},
		"created_at": {tenantID: baseTenant, entry: func() AuditEntry {
			e := base
			e.CreatedAt = base.CreatedAt.Add(time.Nanosecond)
			return e
		}(), prevHash: AuditGenesisHash},
		"prev_hash": {tenantID: baseTenant, entry: base, prevHash: "0000000000000000000000000000000000000000000000000000000000000000"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ComputeAuditRowHash(tc.tenantID, tc.entry, tc.prevHash); got == baseHash {
				t.Errorf("%s: changing this field alone did not change the hash", name)
			}
		})
	}
}

// TestComputeAuditRowHashChains proves the basic chain-building property in
// isolation from any storage: row2's hash, computed with prevHash = row1's
// hash, differs from row2's hash computed with a different prevHash (i.e. the
// hash actually depends on the chain position, not just the row's own
// content), and recomputing row1's hash with the same inputs reproduces it
// exactly (determinism the chain-verify walk depends on).
func TestComputeAuditRowHashChains(t *testing.T) {
	const tenant = "tenant-1"
	row1 := baseAuditEntry()
	row1Hash := ComputeAuditRowHash(tenant, row1, AuditGenesisHash)

	row2 := baseAuditEntry()
	row2.TransactionID = "txn-2"
	row2.CreatedAt = row1.CreatedAt.Add(time.Second)

	row2HashCorrect := ComputeAuditRowHash(tenant, row2, row1Hash)
	row2HashWrongPrev := ComputeAuditRowHash(tenant, row2, AuditGenesisHash)
	if row2HashCorrect == row2HashWrongPrev {
		t.Fatal("row2's hash did not depend on prevHash")
	}

	// Recomputing row1's hash from its own stored fields must reproduce
	// row1Hash exactly: this is what a verify walk relies on.
	if got := ComputeAuditRowHash(tenant, row1, AuditGenesisHash); got != row1Hash {
		t.Fatalf("recomputed row1 hash = %s, want %s", got, row1Hash)
	}
}

// TestComputeAuditRowHashNoBoundaryCollision proves the length-prefixed
// framing keeps adversarial pairs distinct even though a plain
// separator-based scheme would let bytes straddle a field boundary and
// collide (the same property Fingerprint's framing gives Transaction
// content).
func TestComputeAuditRowHashNoBoundaryCollision(t *testing.T) {
	cases := []struct {
		name    string
		tenantA string
		tenantB string
		a, b    AuditEntry
	}{
		{
			name:    "byte moved across actor/transaction_id boundary",
			tenantA: "tenant-1",
			tenantB: "tenant-1",
			a: AuditEntry{
				Action: "act", TransactionID: "ab", Actor: "",
				After: []byte(`{}`), CreatedAt: time.Unix(0, 0).UTC(),
			},
			b: AuditEntry{
				Action: "act", TransactionID: "a", Actor: "b",
				After: []byte(`{}`), CreatedAt: time.Unix(0, 0).UTC(),
			},
		},
		{
			name:    "embedded NUL in actor vs none",
			tenantA: "tenant-1",
			tenantB: "tenant-1",
			a: AuditEntry{
				Action: "act", TransactionID: "t", Actor: "a\x00b",
				After: []byte(`{}`), CreatedAt: time.Unix(0, 0).UTC(),
			},
			b: AuditEntry{
				Action: "act", TransactionID: "t", Actor: "ab",
				After: []byte(`{}`), CreatedAt: time.Unix(0, 0).UTC(),
			},
		},
		{
			// Tenant is the first hashed field, so a byte moved across the
			// tenant/action boundary must not collide either.
			name:    "byte moved across tenant/action boundary",
			tenantA: "ab",
			tenantB: "a",
			a: AuditEntry{
				Action: "", TransactionID: "t", Actor: "x",
				After: []byte(`{}`), CreatedAt: time.Unix(0, 0).UTC(),
			},
			b: AuditEntry{
				Action: "b", TransactionID: "t", Actor: "x",
				After: []byte(`{}`), CreatedAt: time.Unix(0, 0).UTC(),
			},
		},
	}
	for _, tc := range cases {
		if ComputeAuditRowHash(tc.tenantA, tc.a, AuditGenesisHash) == ComputeAuditRowHash(tc.tenantB, tc.b, AuditGenesisHash) {
			t.Errorf("%s: adversarial pair collided", tc.name)
		}
	}
}

func TestComputeAuditRowHash_V1Unchanged(t *testing.T) {
	// A v1 (transaction) entry must hash identically whether HashVersion is 0
	// (unset, legacy) or explicitly 1, and must not depend on subject fields.
	base := AuditEntry{
		Action: ActionTransactionCreated, TransactionID: "tx-1", Actor: "ten-1",
		After: []byte(`{"id":"tx-1"}`), CreatedAt: time.Unix(0, 0).UTC(),
	}
	h0 := ComputeAuditRowHash("ten-1", base, AuditGenesisHash)
	withV1 := base
	withV1.HashVersion = AuditHashV1
	if got := ComputeAuditRowHash("ten-1", withV1, AuditGenesisHash); got != h0 {
		t.Fatalf("v1 explicit hash %s != legacy %s", got, h0)
	}
	withSubject := base
	withSubject.SubjectType = "pending_transaction"
	withSubject.SubjectID = "p-1"
	if got := ComputeAuditRowHash("ten-1", withSubject, AuditGenesisHash); got != h0 {
		t.Fatalf("v1 hash must ignore subject fields, got %s", got)
	}
}

func TestComputeAuditRowHash_V2UsesSubject(t *testing.T) {
	e := AuditEntry{
		Action: "approval.rejected", Actor: "ten-1", HashVersion: AuditHashV2,
		SubjectType: "pending_transaction", SubjectID: "p-1",
		After: []byte(`{"status":"rejected"}`), CreatedAt: time.Unix(0, 0).UTC(),
	}
	h := ComputeAuditRowHash("ten-1", e, AuditGenesisHash)
	tampered := e
	tampered.SubjectID = "p-2"
	if ComputeAuditRowHash("ten-1", tampered, AuditGenesisHash) == h {
		t.Fatal("changing subject_id must change the v2 hash")
	}
	// v2 tolerates an empty TransactionID (rejections have none).
	if e.TransactionID != "" {
		t.Fatal("precondition: v2 rejection has no transaction id")
	}
}

// TestAnchorSignature checks the audit-anchor HMAC (audit remediation): a valid
// signature verifies, and changing any bound field (tenant, chain_seq,
// row_hash), the key, or the signature bytes makes it fail.
func TestAnchorSignature(t *testing.T) {
	key := []byte("anchor-signing-secret")
	tenant := "11111111-1111-1111-1111-111111111111"
	sig := ComputeAnchorSignature(key, tenant, 42, "deadbeef")

	if !VerifyAnchorSignature(key, tenant, 42, "deadbeef", sig) {
		t.Fatal("valid signature did not verify")
	}

	cases := []struct {
		name string
		ok   bool
		run  func() bool
	}{
		{"wrong tenant", false, func() bool {
			return VerifyAnchorSignature(key, "22222222-2222-2222-2222-222222222222", 42, "deadbeef", sig)
		}},
		{"wrong chain_seq", false, func() bool { return VerifyAnchorSignature(key, tenant, 43, "deadbeef", sig) }},
		{"wrong row_hash", false, func() bool { return VerifyAnchorSignature(key, tenant, 42, "cafe", sig) }},
		{"wrong key", false, func() bool {
			return VerifyAnchorSignature([]byte("other-key"), tenant, 42, "deadbeef", sig)
		}},
		{"tampered signature", false, func() bool {
			bad := append([]byte(nil), sig...)
			bad[0] ^= 0xFF
			return VerifyAnchorSignature(key, tenant, 42, "deadbeef", bad)
		}},
		{"empty signature", false, func() bool { return VerifyAnchorSignature(key, tenant, 42, "deadbeef", nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.run(); got != tc.ok {
				t.Errorf("VerifyAnchorSignature = %v, want %v", got, tc.ok)
			}
		})
	}

	// No field-boundary collision: (seq=1, hash="23") must differ from
	// (seq=12, hash="3") even though a naive concat would collide.
	if ComputeAnchorSignatureHex(key, tenant, 1, "23") == ComputeAnchorSignatureHex(key, tenant, 12, "3") {
		t.Error("anchor signature collides across a field boundary")
	}
}

func ComputeAnchorSignatureHex(key []byte, tenant string, seq int64, hash string) string {
	return string(ComputeAnchorSignature(key, tenant, seq, hash))
}
