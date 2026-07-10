-- +goose Up
-- +goose StatementBegin

-- ListTransactions (Task 4.4, audit A7.2) filters on tenant_id and pages
-- newest first by (created_at, id), the same keyset shape AccountStatement
-- uses for postings (migration 0007). Without this index that scan has
-- nothing but transactions_pkey (id) and the (tenant_id, id) uniqueness
-- constraint to work with, neither of which orders by created_at, so every
-- page would need a full tenant-scoped sort. This composite index carries
-- (created_at, id) as trailing columns after tenant_id, so a range scan on
-- the tenant prefix already returns rows in the order the list query needs.
CREATE INDEX transactions_tenant_created_idx
    ON transactions (tenant_id, created_at, id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX transactions_tenant_created_idx;
-- +goose StatementEnd
