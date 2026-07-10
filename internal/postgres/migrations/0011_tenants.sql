-- +goose Up
-- +goose StatementBegin

-- Week 12+ (white-label MVP, ADR-015, Task 2.1): a first-class tenant entity so
-- an operator can suspend or close a tenant. Before this migration "tenant" was
-- only a bare tenant_id uuid scattered across accounts, transactions, api_keys,
-- audit_log, and idempotency_keys, with no row of its own and nothing to gate
-- on. This migration adds the table, backfills one row per tenant_id already
-- referenced by existing data, and only then adds the foreign keys, so the
-- backfill always runs against a fully populated set of ids and the FKs never
-- see a dangling reference.

CREATE TABLE tenants (
    id         uuid        PRIMARY KEY,
    name       text        NOT NULL CHECK (name <> ''),
    status     text        NOT NULL DEFAULT 'active'
                           CHECK (status IN ('active', 'suspended', 'closed')),
    settings   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Backfill one active tenant row for every tenant_id already referenced by the
-- data, so existing tenants (including the default and demo tenants) keep
-- working and the foreign keys below can be added safely. The name is a
-- placeholder ("backfilled-<first 8 hex chars of the id>"): this migration has
-- no other name to give a tenant that has only ever existed as a bare uuid.
INSERT INTO tenants (id, name, status)
SELECT DISTINCT tid, 'backfilled-' || left(tid::text, 8), 'active'
FROM (
    SELECT tenant_id AS tid FROM accounts
    UNION SELECT tenant_id FROM transactions
    UNION SELECT tenant_id FROM api_keys
    UNION SELECT tenant_id FROM audit_log
    UNION SELECT tenant_id FROM idempotency_keys
) t
ON CONFLICT (id) DO NOTHING;

-- Referential integrity: every tenant-owned root row points at a real tenant.
-- Only accounts, transactions, and api_keys get a foreign key: postings and
-- audit_log already reference accounts/transactions (transitively enforcing
-- the same tenant), and idempotency_keys is a control-plane table for a
-- transaction that already carries the constraint.
ALTER TABLE accounts     ADD CONSTRAINT accounts_tenant_fk     FOREIGN KEY (tenant_id) REFERENCES tenants (id);
ALTER TABLE transactions ADD CONSTRAINT transactions_tenant_fk FOREIGN KEY (tenant_id) REFERENCES tenants (id);
ALTER TABLE api_keys     ADD CONSTRAINT api_keys_tenant_fk     FOREIGN KEY (tenant_id) REFERENCES tenants (id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse order: drop the foreign keys before the table they reference, then
-- the table itself. The backfilled rows have no meaning outside this
-- migration's lifetime, so there is nothing else to restore.
ALTER TABLE api_keys     DROP CONSTRAINT api_keys_tenant_fk;
ALTER TABLE transactions DROP CONSTRAINT transactions_tenant_fk;
ALTER TABLE accounts     DROP CONSTRAINT accounts_tenant_fk;

DROP TABLE tenants;

-- +goose StatementEnd
