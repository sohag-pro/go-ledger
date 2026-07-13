-- +goose Up
-- +goose StatementBegin

-- ADR-025: an over-threshold transaction is held here as intent (its
-- original request payload) until an approver decides it. Nothing touches
-- postings/transactions until then, so balances never reflect unapproved
-- money (the core invariant is untouched: it only ever sums postings).
-- transaction_id is set only once, when an approval replays the payload
-- through the ordinary CreateTransaction path and it actually posts.
--
-- kind names which write path the payload will replay on approval (a plain
-- post, an FX convert, or a reversal). status starts 'pending' (the only
-- non-terminal state) and moves to exactly one terminal state: approved,
-- rejected, cancelled, or expired.
--
-- The two CHECK constraints encode the lifecycle: a row is either still
-- pending or has a decided_at timestamp (a decision always stamps one), and
-- transaction_id is only ever set once status is approved (no other
-- terminal state ever posts anything).
CREATE TABLE pending_transactions (
    id             uuid        PRIMARY KEY,
    tenant_id      uuid        NOT NULL REFERENCES tenants (id),
    kind           text        NOT NULL CHECK (kind IN ('post','convert','reverse')),
    payload        json        NOT NULL,
    status         text        NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','approved','rejected','cancelled','expired')),
    threshold_ccy  text        NOT NULL,
    threshold_amt  bigint      NOT NULL,
    created_by     text        NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    decided_by     text,
    decided_at     timestamptz,
    reason         text,
    transaction_id uuid,
    CHECK (status = 'pending' OR decided_at IS NOT NULL),
    CHECK (transaction_id IS NULL OR status = 'approved')
);

-- List/paged view: a tenant's pendings, optionally by status, newest first.
-- The shape GET /v1/pending pages by.
CREATE INDEX pending_transactions_tenant_idx
    ON pending_transactions (tenant_id, created_at DESC, id DESC);
-- The TTL sweep: still-pending rows by age, a partial index so it stays
-- small regardless of how many terminal rows accumulate.
CREATE INDEX pending_transactions_sweep_idx
    ON pending_transactions (created_at) WHERE status = 'pending';

-- Row-level security, consistent with migration 0024/0027/0029 (Task 5.4b,
-- audit A3.5): ENABLE + FORCE + one allow-when-unset tenant_isolation
-- policy. See migration 0024's own doc comment for the full reasoning (the
-- "allow when unset" branch is what lets the background expiry sweep, which
-- runs with no tenant GUC set, read and update across every tenant; FORCE is
-- what stops the owning role from bypassing its own policies).
ALTER TABLE pending_transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE pending_transactions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pending_transactions
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- NO FORCE before DISABLE (migration 0024/0029's own down does the same, for
-- the same reason): relforcerowsecurity is a separate flag from
-- relrowsecurity, and DISABLE ROW LEVEL SECURITY alone does not clear it.
DROP POLICY tenant_isolation ON pending_transactions;
ALTER TABLE pending_transactions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE pending_transactions DISABLE ROW LEVEL SECURITY;

DROP INDEX pending_transactions_sweep_idx;
DROP INDEX pending_transactions_tenant_idx;
DROP TABLE pending_transactions;

-- +goose StatementEnd
