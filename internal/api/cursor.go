package api

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// encodeCursor turns a statement entry's keyset position into an opaque token:
// base64 of "RFC3339Nano|id". Clients pass it back verbatim as ?cursor=.
func encodeCursor(createdAt time.Time, id string) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a token produced by encodeCursor back into a keyset
// position. An empty string means "first page" and returns (nil, nil). A
// malformed token returns an error so the handler can reply 422.
func decodeCursor(token string) (*domain.StatementCursor, error) {
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
