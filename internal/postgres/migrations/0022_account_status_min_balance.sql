-- +goose Up
-- +goose StatementBegin

-- Task 5.5 (audit A1.5): accounts had no lifecycle state and no balance
-- floor. Any account could be driven arbitrarily negative by a valid
-- balanced transaction, and there was no way to put a compliance hold on
-- one. Both new columns are enforced INSIDE the posting transaction
-- (internal/ledger's RunInTx body, before CreateTransaction runs), the same
-- SERIALIZABLE, per-tenant-consistent read the daily-volume policy check
-- (Task 2.4b, migration 0013) already relies on: two concurrent postings
-- that would each individually stay within an account's floor, but together
-- breach it, resolve to exactly one winner, not both slipping through a
-- stale read.

-- status mirrors TenantStatus's two-gate shape (migration 0009, ADR-015):
-- active (default, can post), frozen (a temporary hold, can be reactivated),
-- closed (permanent, not enforced as one-way here either). Every account
-- created before this migration defaults to 'active', so it keeps posting
-- exactly as before.
ALTER TABLE accounts ADD COLUMN status text NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'frozen', 'closed'));

-- min_balance is an optional per-account floor, in the account's own minor
-- units (the same unit every posting amount already uses). NULL (the
-- default, and every existing account) means "no floor": unconstrained,
-- behaving exactly as before this migration. A negative value is a
-- legitimate overdraft allowance (for example -50000 lets a checking
-- account run 500.00 into the red before the floor trips), so there is no
-- CHECK constraining its sign.
ALTER TABLE accounts ADD COLUMN min_balance bigint;

-- Both columns are exempt for system accounts (is_system, migration 0010:
-- the FX clearing accounts, ADR-014), enforced in application code
-- (internal/ledger), not by a CHECK here: a clearing account is expected to
-- carry a permanent, often negative, open position and must never be
-- frozen, so there is nothing for a schema-level constraint to usefully say
-- about it.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE accounts DROP COLUMN min_balance;
ALTER TABLE accounts DROP COLUMN status;
-- +goose StatementEnd
