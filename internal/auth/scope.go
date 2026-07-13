package auth

import (
	"net/http"
	"strings"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// adminPathPrefix is the path prefix that requires domain.ScopeAdmin
// regardless of HTTP method (Task 2.2). There are no routes registered under
// it yet: the admin surface that mints and rotates keys is a separate
// follow-up task (2.2b), but the rule is wired now so those routes are gated
// the moment they exist, rather than needing this middleware touched again.
const adminPathPrefix = "/v1/admin/"

// RequiredHTTPScope returns the domain.Scope an HTTP request needs, based on
// its operation path and method (Task 2.2):
//
//   - any path under adminPathPrefix requires domain.ScopeAdmin, regardless
//     of method;
//   - a path /v1/pending/{id}/approve or /v1/pending/{id}/reject requires
//     domain.ScopeApprove, regardless of method;
//   - a safe method (GET, HEAD, OPTIONS) requires domain.ScopeRead;
//   - every other method (POST, PUT, PATCH, DELETE, and anything else)
//     requires domain.ScopePost, the fail-closed default: an unrecognized
//     method is treated as if it might mutate.
func RequiredHTTPScope(method, path string) domain.Scope {
	if strings.HasPrefix(path, adminPathPrefix) {
		return domain.ScopeAdmin
	}
	// Deciding a pending (approve/reject) needs ScopeApprove; POST would
	// otherwise map to ScopePost. Cancel stays ScopePost (the creator's own
	// key). The list/get GETs fall through to ScopeRead.
	if strings.HasPrefix(path, "/v1/pending/") &&
		(strings.HasSuffix(path, "/approve") || strings.HasSuffix(path, "/reject")) {
		return domain.ScopeApprove
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return domain.ScopeRead
	default:
		return domain.ScopePost
	}
}

// CheckScope reports whether key satisfies required, returning nil if so or a
// *domain.InsufficientScopeError (matched via errors.Is(err,
// domain.ErrInsufficientScope)) if not. Callers map that error to 403
// Forbidden (REST) or codes.PermissionDenied (gRPC), the same shape as the
// tenant-status gate (Task 2.1): the credential is valid, it just lacks the
// scope the operation needs.
func CheckScope(key domain.APIKey, required domain.Scope) error {
	if key.HasScope(required) {
		return nil
	}
	return &domain.InsufficientScopeError{Required: required}
}
