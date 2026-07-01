package domain

import (
	"encoding/json"
	"time"
)

// ActionTransactionCreated is the audit action recorded when a transaction is
// posted. It is the only action in v1: postings are append-only, so there is no
// in-place update to record.
const ActionTransactionCreated = "transaction.created"

// AuditEntry is one immutable row of the audit log. Before is nil for a create.
// After is a JSON snapshot of the entity as of the action. Actor is the tenant
// id until an auth layer resolves a real principal.
type AuditEntry struct {
	ID            string
	Action        string
	TransactionID string
	Actor         string
	Before        json.RawMessage
	After         json.RawMessage
	CreatedAt     time.Time
}
