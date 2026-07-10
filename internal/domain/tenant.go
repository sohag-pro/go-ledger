package domain

import (
	"encoding/json"
	"time"
)

// TenantStatus is a tenant's lifecycle state (ADR-015, Task 2.1: the
// white-label MVP's foundation). Only these three values are valid; every
// other string, including the zero value, is not.
type TenantStatus string

const (
	// TenantActive is the only status that may post or read: a tenant in
	// either other status is gated out at the auth boundary (internal/auth),
	// not by any change to its data.
	TenantActive TenantStatus = "active"
	// TenantSuspended is a temporary gate: an operator can reactivate a
	// suspended tenant by setting its status back to active.
	TenantSuspended TenantStatus = "suspended"
	// TenantClosed is a gate an operator does not expect to reverse. It is
	// otherwise handled identically to suspended: this package does not
	// enforce that a closed tenant can never return to active.
	TenantClosed TenantStatus = "closed"
)

// Valid reports whether s is one of the three defined statuses.
func (s TenantStatus) Valid() bool {
	switch s {
	case TenantActive, TenantSuspended, TenantClosed:
		return true
	default:
		return false
	}
}

// Tenant is a first-class tenant row: the entity an operator suspends or
// closes. Settings is opaque here (its shape is populated in Task 2.4); this
// package only carries it through as raw JSON.
type Tenant struct {
	ID        string
	Name      string
	Status    TenantStatus
	Settings  json.RawMessage
	CreatedAt time.Time
}

// Validate reports whether t is well-formed: a non-empty Name and a valid
// Status. It does not check ID: like Account and Transaction, an empty ID
// means the storage adapter assigns one.
func (t Tenant) Validate() error {
	if t.Name == "" {
		return ErrInvalidTenant
	}
	if !t.Status.Valid() {
		return ErrInvalidTenant
	}
	return nil
}

// TenantNotActiveError is returned when a request is scoped to a tenant whose
// status is not TenantActive. Status carries the tenant's real status
// (suspended or closed) so the transport layer can name the exact reason
// instead of a generic message. It wraps ErrTenantNotActive so callers can
// match with errors.Is(err, ErrTenantNotActive) without caring which status
// applied.
type TenantNotActiveError struct {
	TenantID string
	Status   TenantStatus
}

// Error implements the error interface. It deliberately does not start with
// "domain:" like the package's sentinel errors: it is meant to be read
// directly by an operator or logged as-is, not just matched against.
func (e *TenantNotActiveError) Error() string {
	return "tenant " + e.TenantID + " is " + string(e.Status)
}

// Unwrap lets errors.Is(err, ErrTenantNotActive) match regardless of which
// status caused it.
func (e *TenantNotActiveError) Unwrap() error { return ErrTenantNotActive }

// Reason returns the caller-facing explanation for why the tenant is gated,
// naming the exact status (e.g. "tenant is suspended"). This is what a
// transport layer should put in a 403 / PermissionDenied response body: it
// names the reason without leaking the tenant id to an unauthenticated or
// cross-tenant caller.
func (e *TenantNotActiveError) Reason() string {
	return "tenant is " + string(e.Status)
}
