-- +goose Up
-- +goose StatementBegin

-- Follow-up F1 (audit N1 from the final review): api_keys is tenant-scoped
-- (it carries a tenant_id, migration 0008/0011) but was the one tenant-scoped
-- table migration 0024 (Task 5.4b) left out of row-level security, an
-- inconsistency in that migration's own defense-in-depth story. This closes
-- the gap with the exact same shape: ENABLE + FORCE + one allow-when-unset
-- tenant_isolation policy. See migration 0024's own doc comment for the full
-- reasoning behind the predicate and why FORCE (not just ENABLE) is required.
--
-- Every current reader of api_keys runs with the GUC unset: the auth
-- resolver's GetAPIKeyByHash resolves by globally-unique hash on the pool,
-- the admin surface's ListAPIKeysByTenant/InsertAPIKey/RevokeAPIKey and
-- friends are cross-tenant by design, and cmd/server's boot-time
-- provisionAPIKeys runs before any tenant context exists. None of them go
-- through internal/postgres/repository.go's withTenant/RunInTx. So enabling
-- RLS here changes nothing about how any of them behave today (the
-- allow-when-unset branch of the policy lets them all through exactly as
-- before); it only adds the backstop for a future GUC-set, per-tenant key
-- endpoint that forgets its own WHERE tenant_id.
ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON api_keys
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- NO FORCE before DISABLE (migration 0024/0029's own down does the same,
-- for the same reason): relforcerowsecurity is a separate flag from
-- relrowsecurity, and DISABLE ROW LEVEL SECURITY alone does not clear it.
DROP POLICY tenant_isolation ON api_keys;
ALTER TABLE api_keys NO FORCE ROW LEVEL SECURITY;
ALTER TABLE api_keys DISABLE ROW LEVEL SECURITY;

-- +goose StatementEnd
