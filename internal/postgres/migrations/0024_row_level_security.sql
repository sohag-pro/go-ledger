-- +goose Up
-- +goose StatementBegin

-- Task 5.4b (audit A3.5): Postgres row-level security as defense in depth.
-- The application already scopes every query by tenant_id; this migration
-- is the backstop for a future bug that forgets a WHERE tenant_id (or an
-- UPDATE/DELETE that forgets one): even then, a session cannot read or
-- write across tenants, PROVIDED it is running as a role subject to RLS.
--
-- The predicate: current_setting('app.tenant_id', true) is the session GUC
-- the application sets, transaction-local, at the start of every
-- request-path unit of work (see internal/postgres/repository.go's
-- RunInTx and withTenant). The second argument to current_setting (true)
-- makes it return NULL instead of raising when the GUC was never set,
-- rather than erroring on every query a trusted background worker (or a
-- plain psql session) runs. The policy:
--
--   USING (current_setting('app.tenant_id', true) IS NULL
--          OR current_setting('app.tenant_id', true) = ''
--          OR tenant_id::text = current_setting('app.tenant_id', true))
--
-- allows every row when the GUC is unset or empty, and restricts to the
-- one matching tenant when it is set. This is deliberate, not a loophole:
-- the background workers that legitimately read across every tenant (the
-- audit chainer, the webhook fan-out and delivery worker, the idempotency
-- sweep, restore-verify) never set this GUC, and none of them go through
-- internal/postgres/repository.go, so they keep full cross-tenant access.
-- Only the app request path sets it, and only that path is restricted.
--
-- WITH CHECK repeats the same predicate, so a write cannot INSERT or
-- UPDATE a row into another tenant while the GUC is set, closing the
-- write-side counterpart of the same gap.
--
-- FORCE ROW LEVEL SECURITY matters because the goledger role (in
-- production; the migration-running role in tests) OWNS every one of
-- these tables, and Postgres exempts a table's owner from its own RLS
-- policies unless FORCE is set. Without FORCE, this entire migration would
-- protect nothing: the app's own connection role would sail straight
-- through every policy as the owner. (Superusers bypass RLS unconditionally,
-- with or without FORCE; the app must never connect as one in production.)
ALTER TABLE accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE transactions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON transactions
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE postings ENABLE ROW LEVEL SECURITY;
ALTER TABLE postings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON postings
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON idempotency_keys
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_log
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE audit_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_outbox
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE webhook_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_subscriptions
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_deliveries
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

-- fx_rates.tenant_id is nullable: NULL means a global default rate, visible
-- to every tenant (migration 0014). The predicate adds "tenant_id IS NULL
-- OR" ahead of the usual check, so a global row is always visible and
-- always insertable/updatable regardless of the GUC, while a
-- tenant-specific row follows the same rule every other table uses.
ALTER TABLE fx_rates ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_rates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON fx_rates
    USING (tenant_id IS NULL
           OR current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id IS NULL
           OR current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

-- Deliberately NOT covered: tenants (admin-managed, keyed by id, not a
-- per-tenant-scoped child row: there is no tenant_id column to filter by),
-- and webhook_fanout_cursor (a singleton row with no tenant_id at all).

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- NO FORCE first: relforcerowsecurity is a separate flag from
-- relrowsecurity, and DISABLE ROW LEVEL SECURITY alone does not clear it.
-- Leaving it set would mean a later re-ENABLE (without this migration ever
-- re-running FORCE) still forces the table's owner, an inconsistent
-- half-reverted state.
DROP POLICY tenant_isolation ON fx_rates;
ALTER TABLE fx_rates NO FORCE ROW LEVEL SECURITY;
ALTER TABLE fx_rates DISABLE ROW LEVEL SECURITY;

DROP POLICY tenant_isolation ON webhook_deliveries;
ALTER TABLE webhook_deliveries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries DISABLE ROW LEVEL SECURITY;

DROP POLICY tenant_isolation ON webhook_subscriptions;
ALTER TABLE webhook_subscriptions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE webhook_subscriptions DISABLE ROW LEVEL SECURITY;

DROP POLICY tenant_isolation ON audit_outbox;
ALTER TABLE audit_outbox NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_outbox DISABLE ROW LEVEL SECURITY;

DROP POLICY tenant_isolation ON audit_log;
ALTER TABLE audit_log NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_log DISABLE ROW LEVEL SECURITY;

DROP POLICY tenant_isolation ON idempotency_keys;
ALTER TABLE idempotency_keys NO FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;

DROP POLICY tenant_isolation ON postings;
ALTER TABLE postings NO FORCE ROW LEVEL SECURITY;
ALTER TABLE postings DISABLE ROW LEVEL SECURITY;

DROP POLICY tenant_isolation ON transactions;
ALTER TABLE transactions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE transactions DISABLE ROW LEVEL SECURITY;

DROP POLICY tenant_isolation ON accounts;
ALTER TABLE accounts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE accounts DISABLE ROW LEVEL SECURITY;

-- +goose StatementEnd
