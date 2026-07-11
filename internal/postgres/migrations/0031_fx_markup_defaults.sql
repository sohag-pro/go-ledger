-- +goose Up
-- +goose StatementBegin

-- ADR-020: FX rates and markup become live admin config. spread_bps on a
-- rate row was NOT NULL, forcing the markup onto every pair. It becomes
-- nullable: a non-null value is a per-pair override, NULL means "use the
-- applicable markup default" resolved at conversion time (see the provider).
-- Pre-existing rows keep their concrete value, so no historical conversion
-- changes meaning. The existing CHECK (spread_bps >= 0 AND spread_bps < 10000)
-- is satisfied by NULL and stays as-is.
ALTER TABLE fx_rates ALTER COLUMN spread_bps DROP NOT NULL;

-- fx_markup_defaults holds the default markup a conversion falls back to when
-- a rate row carries no spread. Append-only, mirroring fx_rates (ADR-014):
-- tenant_id NULL is the global default, a non-NULL tenant_id is that tenant's
-- override, effective_at is server-stamped, and a new value is a new row.
CREATE TABLE fx_markup_defaults (
    id                 bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id          uuid        REFERENCES tenants (id),
    default_spread_bps integer     NOT NULL CHECK (default_spread_bps >= 0 AND default_spread_bps < 10000),
    source             text        NOT NULL,
    effective_at       timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- Supports the current-default lookups: latest effective row per scope,
-- tenant tier resolved ahead of global, same shape as fx_rates_current.
CREATE INDEX fx_markup_defaults_current
    ON fx_markup_defaults (tenant_id, effective_at DESC, id DESC);

-- fx_markup_defaults.tenant_id is nullable the same way fx_rates.tenant_id is
-- (NULL means the global default markup, visible to every tenant), so it gets
-- the same tenant_isolation policy: a global row is always visible and
-- insertable regardless of the app.tenant_id GUC, a tenant-specific row
-- follows the usual per-tenant rule (migration 0024).
ALTER TABLE fx_markup_defaults ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_markup_defaults FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON fx_markup_defaults
    USING (tenant_id IS NULL
           OR current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id IS NULL
           OR current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS tenant_isolation ON fx_markup_defaults;
ALTER TABLE fx_markup_defaults NO FORCE ROW LEVEL SECURITY;
ALTER TABLE fx_markup_defaults DISABLE ROW LEVEL SECURITY;
DROP TABLE fx_markup_defaults;
-- Any NULL override must become a concrete 0 before spread_bps can be NOT NULL
-- again, so the down path never fails on existing data.
UPDATE fx_rates SET spread_bps = 0 WHERE spread_bps IS NULL;
ALTER TABLE fx_rates ALTER COLUMN spread_bps SET NOT NULL;
-- +goose StatementEnd
