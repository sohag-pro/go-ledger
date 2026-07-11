package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// disputeFromRow builds a domain.Dispute from a sqlc.Dispute row, shared by
// CreateDispute (via GetDispute is not needed there), GetDispute,
// ListDisputes, and ResolveDispute: every one of those selects the identical
// column set.
func disputeFromRow(row sqlc.Dispute) domain.Dispute {
	d := domain.Dispute{
		ID:            row.ID.String(),
		TenantID:      row.TenantID.String(),
		TransactionID: row.TransactionID.String(),
		Status:        domain.DisputeStatus(row.Status),
		Reason:        row.Reason,
		CreatedAt:     row.CreatedAt,
	}
	if row.ResolutionTransactionID.Valid {
		id := uuid.UUID(row.ResolutionTransactionID.Bytes).String()
		d.ResolutionTransactionID = &id
	}
	if row.ResolvedAt.Valid {
		t := row.ResolvedAt.Time
		d.ResolvedAt = &t
	}
	return d
}

// CreateDispute assigns an identity if d.ID is empty, validates d, and
// inserts it (Task 6.3, audit A9.2). The caller (ledger.DisputeService) is
// expected to have already confirmed d.TransactionID names a real
// transaction within tenantID; the composite FK (migration 0029,
// disputes_txn_fk) enforces it here too.
func (r *Repository) CreateDispute(ctx context.Context, tenantID string, d *domain.Dispute) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if d.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("postgres: generate dispute id: %w", err)
		}
		d.ID = id.String()
	}
	if d.Status == "" {
		d.Status = domain.DisputeOpen
	}
	d.TenantID = tenantID
	if err := d.Validate(); err != nil {
		return err
	}
	did, err := uuid.Parse(d.ID)
	if err != nil {
		return fmt.Errorf("postgres: parse dispute id: %w", err)
	}
	txID, err := uuid.Parse(d.TransactionID)
	if err != nil {
		return fmt.Errorf("postgres: parse dispute transaction id: %w", err)
	}
	return r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		return q.CreateDispute(ctx, sqlc.CreateDisputeParams{
			ID:            did,
			TenantID:      tid,
			TransactionID: txID,
			Reason:        d.Reason,
		})
	})
}

// GetDispute returns the dispute, or domain.ErrDisputeNotFound if absent
// (Task 6.3, audit A9.2).
func (r *Repository) GetDispute(ctx context.Context, tenantID, id string) (domain.Dispute, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.Dispute{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	did, err := uuid.Parse(id)
	if err != nil {
		return domain.Dispute{}, fmt.Errorf("postgres: parse dispute id: %w", err)
	}
	var row sqlc.Dispute
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetDispute(ctx, sqlc.GetDisputeParams{TenantID: tid, ID: did})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Dispute{}, domain.ErrDisputeNotFound
	}
	if err != nil {
		return domain.Dispute{}, fmt.Errorf("postgres: get dispute: %w", err)
	}
	return disputeFromRow(row), nil
}

// ListDisputes returns up to limit of the tenant's disputes, newest first,
// keyset paged, optionally filtered by status (Task 6.3, audit A9.2).
func (r *Repository) ListDisputes(ctx context.Context, tenantID string, status *domain.DisputeStatus, after *domain.StatementCursor, limit int) ([]domain.Dispute, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}

	afterTime, afterID := statementFirstPageTime, uuid.Max
	if after != nil {
		afterTime = after.CreatedAt
		if afterID, err = uuid.Parse(after.ID); err != nil {
			return nil, fmt.Errorf("postgres: parse cursor id: %w", err)
		}
	}

	var statusFilter pgtype.Text
	if status != nil {
		statusFilter = pgtype.Text{String: string(*status), Valid: true}
	}

	var rows []sqlc.Dispute
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListDisputes(ctx, sqlc.ListDisputesParams{
			TenantID:       tid,
			Status:         statusFilter,
			AfterCreatedAt: afterTime,
			AfterID:        afterID,
			PageLimit:      int32(limit), //nolint:gosec // limit is bounded by the API layer
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list disputes: %w", err)
	}
	out := make([]domain.Dispute, 0, len(rows))
	for _, row := range rows {
		out = append(out, disputeFromRow(row))
	}
	return out, nil
}

// ResolveDispute transitions the dispute from open to status, stamping
// resolution_transaction_id and resolved_at (Task 6.3, audit A9.2). It
// returns domain.ErrDisputeAlreadyResolved if the guarded UPDATE (WHERE
// status = 'open') affected zero rows because the dispute is not currently
// open, or domain.ErrDisputeNotFound if no dispute matches id within
// tenantID at all: a second read (GetDispute) after the zero-row UPDATE
// distinguishes the two, since the guarded UPDATE alone cannot tell "does
// not exist" from "exists but already resolved".
func (r *Repository) ResolveDispute(ctx context.Context, tenantID, id string, status domain.DisputeStatus, resolutionTransactionID *string) (domain.Dispute, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return domain.Dispute{}, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	did, err := uuid.Parse(id)
	if err != nil {
		return domain.Dispute{}, fmt.Errorf("postgres: parse dispute id: %w", err)
	}
	var resolutionID pgtype.UUID
	if resolutionTransactionID != nil {
		rid, err := uuid.Parse(*resolutionTransactionID)
		if err != nil {
			return domain.Dispute{}, fmt.Errorf("postgres: parse resolution transaction id: %w", err)
		}
		resolutionID = pgtype.UUID{Bytes: rid, Valid: true}
	}

	var row sqlc.Dispute
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.ResolveDispute(ctx, sqlc.ResolveDisputeParams{
			TenantID:                tid,
			ID:                      did,
			Status:                  string(status),
			ResolutionTransactionID: resolutionID,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Zero rows: either no such dispute at all, or it exists but is no
		// longer open. GetDispute (a separate withTenant call, outside the
		// guarded UPDATE's own transaction) tells the two apart.
		if _, getErr := r.GetDispute(ctx, tenantID, id); errors.Is(getErr, domain.ErrDisputeNotFound) {
			return domain.Dispute{}, domain.ErrDisputeNotFound
		}
		return domain.Dispute{}, domain.ErrDisputeAlreadyResolved
	}
	if err != nil {
		return domain.Dispute{}, fmt.Errorf("postgres: resolve dispute: %w", err)
	}
	return disputeFromRow(row), nil
}
