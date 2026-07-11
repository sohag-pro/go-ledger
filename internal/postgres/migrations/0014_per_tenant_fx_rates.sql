-- +goose Up
-- +goose StatementBegin

-- Task 2.4 (audit A3.3): let a tenant carry its own FX rate and spread for a
-- pair, falling back to the global default (fx_rates.tenant_id NULL) when it
-- has none. Before this migration fx_rates was entirely global: every
-- tenant resolved the exact same mid rate and spread for a given pair, with
-- no way to negotiate a better (or worse) spread with one tenant without
-- affecting every other tenant.
--
-- tenant_id is nullable and added with no backfill: every row that exists
-- before this migration keeps tenant_id NULL, which is exactly what "the
-- global default rate" means, so existing resolution behavior (CurrentFXRate
-- in internal/postgres/queries/fx_rates.sql) is unchanged until an operator
-- actually inserts a tenant-specific row.
ALTER TABLE fx_rates ADD COLUMN tenant_id uuid REFERENCES tenants (id);

-- Supports CurrentFXRate's tenant-aware lookup: WHERE (tenant_id = $1 OR
-- tenant_id IS NULL) AND base = $2 AND quote = $3, ordered by effective_at
-- DESC. Leading on tenant_id lets a tenant-specific lookup and a
-- global-only lookup (tenant_id IS NULL) each use the same index instead of
-- a sequential scan over every rate ever quoted.
CREATE INDEX fx_rates_tenant_lookup_idx
    ON fx_rates (tenant_id, base, quote, effective_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX fx_rates_tenant_lookup_idx;
ALTER TABLE fx_rates DROP COLUMN tenant_id;

-- +goose StatementEnd
