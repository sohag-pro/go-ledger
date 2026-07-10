package domain

import (
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
}

// ComputeAuditRowHash returns the hex SHA-256 digest chaining tenantID and e's
// content to prevHash, the previous audit row's RowHash for the same tenant (or
// AuditGenesisHash for a tenant's first row). Recomputing this from a row's
// stored tenant id, its other stored fields, and its predecessor's stored
// RowHash must reproduce the row's stored RowHash exactly; any mismatch means
// the row (or one before it) was altered after the fact.
//
// The hashed fields, in this exact order, are:
//
//  1. tenantID
//  2. Action
//  3. TransactionID
//  4. Actor
//  5. Before (raw bytes, nil for a create)
//  6. After (raw bytes)
//  7. CreatedAt, encoded as its UnixNano decimal string
//  8. prevHash
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
// tamper-evident audit chain").
func ComputeAuditRowHash(tenantID string, e AuditEntry, prevHash string) string {
	h := sha256.New()
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
