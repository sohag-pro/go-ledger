-- +goose Up
-- +goose StatementBegin

-- The statement and audit-by-account queries filter on (tenant_id, account_id)
-- and then scan ordered by (created_at, id) to page newest first (ADR-006). The
-- old postings_tenant_account_idx only covers the equality filter, so each page
-- still needed a sort. This composite index carries (created_at, id) as trailing
-- columns, so an index range scan on the tenant/account prefix already returns
-- rows in the order the statement and audit-by-account queries need, removing
-- the per-page sort. It also still serves the plain equality filter used by the
-- balance SUM, since (tenant_id, account_id) remains its leading prefix.
CREATE INDEX postings_tenant_account_created_idx
    ON postings (tenant_id, account_id, created_at, id);

-- Redundant now: any query that could use the old two-column index can use the
-- new composite's leading prefix instead, and the composite additionally serves
-- the ordered scans. Keeping both would just double the write-side index
-- maintenance cost for no read benefit.
DROP INDEX postings_tenant_account_idx;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE INDEX postings_tenant_account_idx ON postings (tenant_id, account_id);
DROP INDEX postings_tenant_account_created_idx;
-- +goose StatementEnd
