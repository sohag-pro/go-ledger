// Package paging holds the shared keyset cursor used by both the REST and gRPC
// adapters to page account statements and audit logs, so the two protocols
// speak the identical opaque cursor format.
package paging

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// EncodeCursor turns a keyset position into an opaque token: base64 of
// "RFC3339Nano|id". Clients pass it back verbatim.
func EncodeCursor(createdAt time.Time, id string) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a token produced by EncodeCursor. An empty string means
// "first page" and returns (nil, nil). A malformed token returns an error.
func DecodeCursor(token string) (*domain.StatementCursor, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil, fmt.Errorf("invalid cursor format")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor timestamp: %w", err)
	}
	return &domain.StatementCursor{CreatedAt: createdAt, ID: id}, nil
}

// Page trims rows to at most limit, reporting whether there is a next page.
// The caller is expected to have requested limit+1 rows from the repository
// (Task 4.4, audit A7.2): getting back more than limit rows means there is a
// next page, detected without a second round trip. This is more precise than
// the older "a full page implies more" heuristic AccountStatement and
// ListAuditByAccount still use (see internal/api/accounts.go,
// internal/api/audit.go): that heuristic occasionally hands back a
// next_cursor for what turns out to be the very last page, costing the
// caller one extra, empty request; requesting limit+1 up front avoids that
// edge case entirely.
func Page[T any](rows []T, limit int) (page []T, hasMore bool) {
	if limit > 0 && len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}
