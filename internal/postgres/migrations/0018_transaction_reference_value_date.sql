-- +goose Up
-- +goose StatementBegin

-- reference is an optional, client-supplied external id for reconciliation
-- against an upstream system (Task 4.3, audit A1.3): a bank statement line, a
-- payment processor's charge id, that sort of thing. Nullable: most posts
-- have no external system to reconcile against at all.
ALTER TABLE transactions ADD COLUMN reference text;

-- effective_at is the value date: when the transaction is considered to have
-- happened economically, as distinct from created_at (when the row was
-- actually written). Nullable: NULL means "no value date supplied", read back
-- as created_at by the application (see Repository.transactionFromRow), not
-- backfilled here, so a transaction posted before this migration and one
-- posted after with no effective_at look identical on disk.
ALTER TABLE transactions ADD COLUMN effective_at timestamptz;

-- A reference is unique within a tenant (a caller's own idempotent-ish
-- external id): two different tenants may each use "INV-1001" for their own
-- unrelated purposes, and the same tenant may post many transactions with no
-- reference at all. The partial WHERE clause is what makes both of those
-- true: only a non-NULL reference is constrained, so NULLs never collide with
-- each other or count against the unique index.
CREATE UNIQUE INDEX transactions_tenant_reference_idx
    ON transactions (tenant_id, reference) WHERE reference IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX transactions_tenant_reference_idx;
ALTER TABLE transactions DROP COLUMN effective_at;
ALTER TABLE transactions DROP COLUMN reference;

-- +goose StatementEnd
