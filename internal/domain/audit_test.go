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
	h1 := ComputeAuditRowHash(e, AuditGenesisHash)
	h2 := ComputeAuditRowHash(e, AuditGenesisHash)
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
	baseHash := ComputeAuditRowHash(base, AuditGenesisHash)

	cases := map[string]struct {
		entry    AuditEntry
		prevHash string
	}{
		"action":         {entry: func() AuditEntry { e := base; e.Action = "transaction.other"; return e }(), prevHash: AuditGenesisHash},
		"transaction id": {entry: func() AuditEntry { e := base; e.TransactionID = "txn-2"; return e }(), prevHash: AuditGenesisHash},
		"actor":          {entry: func() AuditEntry { e := base; e.Actor = "tenant-2"; return e }(), prevHash: AuditGenesisHash},
		"before": {entry: func() AuditEntry {
			e := base
			e.Before = []byte(`{"was":"something"}`)
			return e
		}(), prevHash: AuditGenesisHash},
		"after": {entry: func() AuditEntry {
			e := base
			e.After = []byte(`{"id":"txn-1","extra":true}`)
			return e
		}(), prevHash: AuditGenesisHash},
		"created_at": {entry: func() AuditEntry {
			e := base
			e.CreatedAt = base.CreatedAt.Add(time.Nanosecond)
			return e
		}(), prevHash: AuditGenesisHash},
		"prev_hash": {entry: base, prevHash: "0000000000000000000000000000000000000000000000000000000000000000"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ComputeAuditRowHash(tc.entry, tc.prevHash); got == baseHash {
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
	row1 := baseAuditEntry()
	row1Hash := ComputeAuditRowHash(row1, AuditGenesisHash)

	row2 := baseAuditEntry()
	row2.TransactionID = "txn-2"
	row2.CreatedAt = row1.CreatedAt.Add(time.Second)

	row2HashCorrect := ComputeAuditRowHash(row2, row1Hash)
	row2HashWrongPrev := ComputeAuditRowHash(row2, AuditGenesisHash)
	if row2HashCorrect == row2HashWrongPrev {
		t.Fatal("row2's hash did not depend on prevHash")
	}

	// Recomputing row1's hash from its own stored fields must reproduce
	// row1Hash exactly: this is what a verify walk relies on.
	if got := ComputeAuditRowHash(row1, AuditGenesisHash); got != row1Hash {
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
		name string
		a, b AuditEntry
	}{
		{
			name: "byte moved across actor/transaction_id boundary",
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
			name: "embedded NUL in actor vs none",
			a: AuditEntry{
				Action: "act", TransactionID: "t", Actor: "a\x00b",
				After: []byte(`{}`), CreatedAt: time.Unix(0, 0).UTC(),
			},
			b: AuditEntry{
				Action: "act", TransactionID: "t", Actor: "ab",
				After: []byte(`{}`), CreatedAt: time.Unix(0, 0).UTC(),
			},
		},
	}
	for _, tc := range cases {
		if ComputeAuditRowHash(tc.a, AuditGenesisHash) == ComputeAuditRowHash(tc.b, AuditGenesisHash) {
			t.Errorf("%s: adversarial pair collided", tc.name)
		}
	}
}
