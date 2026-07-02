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
