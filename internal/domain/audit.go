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

// AuditGenesisHash is the prev_hash of a tenant's first audit row: the empty
// string. There is no prior row to chain from, so genesis is "nothing", not a
// fixed magic value that could be mistaken for a real hash.
const AuditGenesisHash = ""

// AuditEntry is one immutable row of the audit log. Before is nil for a create.
// After is a JSON snapshot of the entity as of the action. Actor is the tenant
// id until an auth layer resolves a real principal.
//
// PrevHash and RowHash carry the per-tenant tamper-evident hash chain (ADR-012):
// RowHash is ComputeAuditRowHash(entry, PrevHash), and PrevHash is the previous
// row's RowHash for the same tenant (or AuditGenesisHash for the first row).
// The storage adapter computes both inside the same transaction that extends
// the chain; callers building an AuditEntry to append do not set them.
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

// ComputeAuditRowHash returns the hex SHA-256 digest chaining e's content to
// prevHash, the previous audit row's RowHash for the same tenant (or
// AuditGenesisHash for a tenant's first row). Recomputing this from a row's
// stored fields and its predecessor's stored RowHash must reproduce the row's
// stored RowHash exactly; any mismatch means the row (or one before it) was
// altered after the fact.
//
// The hashed fields, in this exact order, are:
//
//  1. Action
//  2. TransactionID
//  3. Actor
//  4. Before (raw bytes, nil for a create)
//  5. After (raw bytes)
//  6. CreatedAt, encoded as its UnixNano decimal string
//  7. prevHash
//
// Each field is length-prefixed before hashing (the same self-delimiting
// framing Transaction.Fingerprint uses via writeField), so no field's bytes
// can be mistaken for a field boundary. CreatedAt is encoded as UnixNano rather
// than a formatted timestamp string to avoid any ambiguity from timezone or
// sub-second formatting; it must be the exact value the caller is about to
// persist in the row, not a value computed separately, or the stored hash will
// never recompute correctly.
//
// The tenant id is deliberately not part of the hashed content: AuditEntry
// does not carry one (it is a parameter to the repository methods, not a
// struct field), and the chain is already scoped per tenant structurally: it
// is extended and verified by reading only that tenant's rows in created_at,
// id order, and prevHash must match that tenant's actual previous row_hash for
// the chain to extend. See ADR-012 ("A per-tenant, tamper-evident audit
// chain").
func ComputeAuditRowHash(e AuditEntry, prevHash string) string {
	h := sha256.New()
	writeField(h, []byte(e.Action))
	writeField(h, []byte(e.TransactionID))
	writeField(h, []byte(e.Actor))
	writeField(h, e.Before)
	writeField(h, e.After)
	writeField(h, []byte(strconv.FormatInt(e.CreatedAt.UnixNano(), 10)))
	writeField(h, []byte(prevHash))
	return hex.EncodeToString(h.Sum(nil))
}
