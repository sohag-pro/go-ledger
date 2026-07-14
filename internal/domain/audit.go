package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// ActionTransactionCreated is the audit action recorded when a transaction is
// posted. It is the only action in v1: postings are append-only, so there is no
// in-place update to record.
const ActionTransactionCreated = "transaction.created"

// ActionTransactionReversed is the audit action recorded when
// TransactionService.ReverseTransaction posts a reversal (Task 4.2, audit
// A1.2). It is recorded once, for the reversal transaction itself, at the
// moment it is first posted; a replayed (already-reversed) call posts no new
// audit event, since no new transaction was written.
const ActionTransactionReversed = "transaction.reversed"

// AuditGenesisHash is the prev_hash of a tenant's first audit row: the empty
// string. There is no prior row to chain from, so genesis is "nothing", not a
// fixed magic value that could be mistaken for a real hash.
const AuditGenesisHash = ""

// Audit row-hash scheme versions (ADR-025). v1 is the original
// transaction-only preimage; v2 adds subject_type/subject_id and tolerates a
// null transaction id, so the chain can carry non-transaction lifecycle
// events. A row records its version so verification recomputes with the
// matching preimage and every existing (v1) row stays reproducible.
const (
	AuditHashV1 = 1
	AuditHashV2 = 2
)

// AuditEntry is one immutable row of the audit log. Before is nil for a create.
// After is a JSON snapshot of the entity as of the action. Actor is the tenant
// id until an auth layer resolves a real principal.
//
// PrevHash and RowHash carry the per-tenant tamper-evident hash chain (ADR-012):
// RowHash is ComputeAuditRowHash(tenantID, entry, PrevHash), and PrevHash is the
// previous row's RowHash for the same tenant (or AuditGenesisHash for the first
// row). Since ADR-017 (the multi-instance audit chain) these are computed by
// the single background chainer, not by the transaction that posts the event:
// a post writes an AuditEvent to the outbox instead, and the chainer is what
// eventually produces the AuditEntry these two fields describe.
// ChainSeq is populated only by ListAuditForVerifyPage (Task 5.3): the row's
// position in true chain-insertion order (ADR-017, migration 0016), used to
// page and to resume a walk from a trusted checkpoint
// (VerifyFromLatestAnchor). It is never hashed (ComputeAuditRowHash does not
// read it) and is left zero on every other read of an AuditEntry
// (ByTransaction, ByAccount, and ListAuditForVerify, which predates paging
// and has no need to advance a cursor).
type AuditEntry struct {
	ID            string
	Action        string
	TransactionID string
	Actor         string
	Before        json.RawMessage
	After         json.RawMessage
	CreatedAt     time.Time
	PrevHash      string
	RowHash       string
	ChainSeq      int64

	// SubjectType, SubjectID, and HashVersion carry the v2 row-hash scheme
	// (ADR-025). SubjectType/SubjectID identify the entity a non-transaction
	// lifecycle event (e.g. an approval decision) is about, since
	// TransactionID may be empty for those. HashVersion selects which
	// preimage ComputeAuditRowHash uses: 0 (unset, legacy) or 1 means the
	// original transaction-only preimage; 2 means the subject-aware preimage.
	SubjectType string
	SubjectID   string
	HashVersion int
}

// AuditAnchor is one recorded off-box checkpoint of a tenant's chain head
// (Task 5.3, migration 0025): the chain_seq and row_hash the anchor job read
// and logged at CreatedAt. See internal/audit.AnchorJob and
// AuditService.VerifyFromLatestAnchor for how it is produced and consumed.
type AuditAnchor struct {
	ChainSeq  int64
	RowHash   string
	CreatedAt time.Time
	// Signature is the app-held HMAC over (tenant_id, chain_seq, row_hash),
	// or nil when signing is disabled. See ComputeAnchorSignature.
	Signature []byte
}

// ComputeAnchorSignature returns the HMAC-SHA256 of an anchor's identity
// (tenantID, chainSeq, rowHash), keyed by key. Fields are length-prefixed with
// the same writeField helper ComputeAuditRowHash uses, so no field-boundary
// ambiguity lets one anchor's preimage collide with another's.
//
// This is what makes an audit anchor tamper-evident against a DB-privileged
// attacker (audit remediation): audit_anchors lives in the database, so a
// rewrite of audit_log plus a matching rewrite of audit_anchors would otherwise
// pass VerifyFromLatestAnchor. The key is held by the application
// (AUDIT_ANCHOR_SIGNING_KEY), NOT by the database role, so an attacker with
// only DB access cannot forge a valid signature for the rewritten head.
func ComputeAnchorSignature(key []byte, tenantID string, chainSeq int64, rowHash string) []byte {
	mac := hmac.New(sha256.New, key)
	writeField(mac, []byte(tenantID))
	writeField(mac, []byte(strconv.FormatInt(chainSeq, 10)))
	writeField(mac, []byte(rowHash))
	return mac.Sum(nil)
}

// VerifyAnchorSignature reports whether sig is a valid ComputeAnchorSignature
// for the given anchor identity under key, using a constant-time compare.
func VerifyAnchorSignature(key []byte, tenantID string, chainSeq int64, rowHash string, sig []byte) bool {
	return hmac.Equal(sig, ComputeAnchorSignature(key, tenantID, chainSeq, rowHash))
}

// AuditEvent is the payload a post or convert writes to the audit outbox
// (ADR-017), inside the same transaction that writes the postings. It is a
// deliberately smaller shape than AuditEntry: only what the caller knows at
// post time. It has no ID, CreatedAt, PrevHash, or RowHash, because none of
// those are post-time concerns anymore: the single background chainer
// (internal/audit.Chainer) assigns them later, in transaction-commit order,
// reading occurred_at back from the persisted outbox row rather than
// recomputing a timestamp of its own (see the chainer's doc comment for why
// that is what keeps row_hash reproducible).
type AuditEvent struct {
	Action        string
	TransactionID string
	Actor         string
	Before        json.RawMessage
	After         json.RawMessage

	// SubjectType, SubjectID, and HashVersion mirror the same fields on
	// AuditEntry (see its doc comment); they let a non-transaction lifecycle
	// event flow through the outbox to the chainer, which copies them onto
	// the AuditEntry it builds and hashes.
	SubjectType string
	SubjectID   string
	HashVersion int
}

// ComputeAuditRowHash returns the hex SHA-256 digest chaining tenantID and e's
// content to prevHash, the previous audit row's RowHash for the same tenant (or
// AuditGenesisHash for a tenant's first row). Recomputing this from a row's
// stored tenant id, its other stored fields, and its predecessor's stored
// RowHash must reproduce the row's stored RowHash exactly; any mismatch means
// the row (or one before it) was altered after the fact.
//
// e.HashVersion selects which preimage is hashed (ADR-025).
//
// When e.HashVersion is 0 (unset, every row written before ADR-025) or
// AuditHashV1, the hashed fields are, in this exact order: tenantID, Action,
// TransactionID, Actor, Before (raw bytes, nil for a create), After (raw
// bytes), CreatedAt (its UnixNano decimal string), prevHash. This branch does
// not read SubjectType or SubjectID at all, so it is unaffected by those
// fields being set; every row stored before ADR-025 recomputes to its stored
// hash unchanged.
//
// When e.HashVersion is AuditHashV2, the same preimage carries SubjectType and
// SubjectID right after Action and ahead of TransactionID, which is tolerated
// empty for a non-transaction lifecycle event (e.g. an approval decision):
// tenantID, Action, SubjectType, SubjectID, TransactionID (empty string when
// the event has none), Actor, Before, After, CreatedAt, prevHash.
//
// Each field is length-prefixed before hashing (the same self-delimiting
// framing Transaction.Fingerprint uses via writeField), so no field's bytes
// can be mistaken for a field boundary. CreatedAt is encoded as UnixNano rather
// than a formatted timestamp string to avoid any ambiguity from timezone or
// sub-second formatting; it must be the exact value the caller is about to
// persist in the row, not a value computed separately, or the stored hash will
// never recompute correctly.
//
// The tenant id is hashed first, ahead of Action, so that a database-privileged
// rewrite of a row's tenant_id (moving a row into another tenant's chain) is
// detectable: recomputing the hash with the row's current (rewritten) tenant id
// no longer matches the row_hash that was stored under the original tenant id.
// AuditEntry itself does not carry a tenant id field because it is a parameter
// to the repository methods, not a struct field; the chain is also scoped per
// tenant structurally, extended and verified by reading only that tenant's rows
// in created_at, id order, with prevHash required to match that tenant's actual
// previous row_hash for the chain to extend. See ADR-012 ("A per-tenant,
// tamper-evident audit chain") and ADR-025 ("Versioned audit-row hashing").
func ComputeAuditRowHash(tenantID string, e AuditEntry, prevHash string) string {
	h := sha256.New()
	if e.HashVersion == AuditHashV2 {
		// v2: fold in the subject and tolerate an empty transaction id.
		writeField(h, []byte(tenantID))
		writeField(h, []byte(e.Action))
		writeField(h, []byte(e.SubjectType))
		writeField(h, []byte(e.SubjectID))
		writeField(h, []byte(e.TransactionID))
		writeField(h, []byte(e.Actor))
		writeField(h, e.Before)
		writeField(h, e.After)
		writeField(h, []byte(strconv.FormatInt(e.CreatedAt.UnixNano(), 10)))
		writeField(h, []byte(prevHash))
		return hex.EncodeToString(h.Sum(nil))
	}
	// v1 (HashVersion 0 or 1): the exact original preimage. Unchanged, so
	// every already-stored transaction row recomputes to its stored hash.
	writeField(h, []byte(tenantID))
	writeField(h, []byte(e.Action))
	writeField(h, []byte(e.TransactionID))
	writeField(h, []byte(e.Actor))
	writeField(h, e.Before)
	writeField(h, e.After)
	writeField(h, []byte(strconv.FormatInt(e.CreatedAt.UnixNano(), 10)))
	writeField(h, []byte(prevHash))
	return hex.EncodeToString(h.Sum(nil))
}
