-- +goose Up
-- +goose StatementBegin

-- Task 5.3 (audit A2.4): off-box anchoring for the tamper-evident audit
-- chain (ADR-012, ADR-017). A hash chain proves internal self-consistency:
-- recomputing every row's hash from its own content and its predecessor's
-- stored hash catches a row whose content was changed without also
-- correctly updating every hash from that point forward. It does NOT catch
-- a privileged actor with full database write access who rewrites a whole
-- suffix of the chain consistently (new content, correctly recomputed
-- row_hash and prev_hash all the way to the current head): that rewritten
-- suffix is, by construction, internally self-consistent again, and no
-- amount of re-walking the chain from genesis will ever reveal it.
--
-- audit_anchors is the record that closes that gap. Periodically (the
-- anchor job, internal/audit.AnchorJob), for every tenant, the current head
-- (chain_seq, row_hash) is written here AND logged at info level in
-- structured JSON that an off-box log shipper (Task 5.6) captures outside
-- this database's control. A later rewrite of an already-anchored row
-- changes its row_hash; comparing the live head against the last anchor (or
-- the shipped log line, once it is off this box entirely) reveals the
-- rewrite even when the chain re-verifies clean on its own. See the Task 5.3
-- report for the full trust model: the anchored prefix is TRUSTED as a
-- checkpoint (the off-box copy is the ground truth for it), not re-proven by
-- every from-anchor verify; a full Verify from genesis is what still proves
-- the whole chain, including everything before the earliest anchor.
CREATE TABLE audit_anchors (
    id         bigserial   PRIMARY KEY,
    tenant_id  uuid        NOT NULL,
    chain_seq  bigint      NOT NULL,           -- the head chain_seq at anchor time
    row_hash   text        NOT NULL,           -- the head row_hash at anchor time
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_anchors_tenant_seq_idx ON audit_anchors (tenant_id, chain_seq DESC);

-- RLS consistent with migration 0024 (Task 5.4b, audit A3.5): allow every
-- row when the app.tenant_id GUC is unset or empty (the anchor job's own
-- cross-tenant reads and writes, which never set it), restrict to the
-- matching tenant when it is set (the verify request path, reading its own
-- tenant's latest anchor). FORCE is required for the same reason migration
-- 0024's own comment explains: the owning role must not be exempt from its
-- own policy.
ALTER TABLE audit_anchors ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_anchors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_anchors
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- NO FORCE before DISABLE (the 0024 lesson): relforcerowsecurity is a
-- separate flag from relrowsecurity, and DISABLE alone does not clear it.
DROP POLICY tenant_isolation ON audit_anchors;
ALTER TABLE audit_anchors NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_anchors DISABLE ROW LEVEL SECURITY;
DROP TABLE audit_anchors;

-- +goose StatementEnd
