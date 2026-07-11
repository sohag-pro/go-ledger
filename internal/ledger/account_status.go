package ledger

import (
	"context"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// enforceAccountConstraints checks postings against each touched account's
// status and optional minimum balance (Task 5.5, audit A1.5). It is called
// from inside RunInTx, in both Post and Convert, before CreateTransaction,
// the same placement enforceTenantPolicy already uses and for the same
// reason: the balance read must be consistent with the write that follows
// it, and RunInTx's SERIALIZABLE transaction is what makes that read race
// free (two concurrent same-tenant postings that would each individually
// keep an account above its floor, but together breach it, are a genuine
// read-write antidependency SERIALIZABLE detects and aborts one of).
//
// Unlike enforceTenantPolicy's TenantDailyDebits read, there is no "only
// call this when something is actually configured" shortcut here: every
// posting touches at least two accounts (Transaction.Validate), and any one
// of them might carry a status or floor, so the states read always runs.
// It is a single query keyed on this transaction's own distinct account
// ids (never a query per account), the same batching
// ListPostingsByTransactionIDs already uses for a page of transactions.
func enforceAccountConstraints(ctx context.Context, tx domain.Tx, tenantID string, postings []domain.Posting) error {
	ids := distinctAccountIDs(postings)
	states, err := tx.AccountPostingStates(ctx, tenantID, ids)
	if err != nil {
		return err
	}
	return domain.CheckAccountPostingConstraints(states, postings)
}

// distinctAccountIDs returns the distinct account ids postings touches, in
// first-seen order. Order does not matter to the caller (AccountPostingStates
// returns a map), but a deterministic, duplicate-free slice keeps the query's
// ANY($2) argument small and stable across otherwise-identical retries.
func distinctAccountIDs(postings []domain.Posting) []string {
	seen := make(map[string]struct{}, len(postings))
	ids := make([]string, 0, len(postings))
	for _, p := range postings {
		if _, ok := seen[p.AccountID]; ok {
			continue
		}
		seen[p.AccountID] = struct{}{}
		ids = append(ids, p.AccountID)
	}
	return ids
}
