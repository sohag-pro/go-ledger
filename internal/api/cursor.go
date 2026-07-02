package api

import (
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/paging"
)

func encodeCursor(createdAt time.Time, id string) string {
	return paging.EncodeCursor(createdAt, id)
}

func decodeCursor(token string) (*domain.StatementCursor, error) {
	return paging.DecodeCursor(token)
}
