-- +goose Up
-- +goose StatementBegin

-- Task 6.3 (audit A9.2): a dispute/chargeback data model built on the
-- reversal primitive (Task 4.2). Opening a dispute records intent only, no
-- money moves; resolving one either posts a real reversal (through
-- ledger.TransactionService.ReverseTransaction, never a raw insert here) or
-- rejects the dispute with no money movement at all.
--
-- resolution_transaction_id names the reversal actually posted on
-- resolution, if any: nullable, since a rejected dispute never gets one.
-- There is deliberately no foreign key on it: the id it carries IS the
-- transactions row ReverseTransaction just created, but adding a second FK
-- here (on top of the transaction_id one below) would only duplicate a
-- guarantee the application already provides by construction, the same
-- reasoning migration 0021 gives for not adding one on webhook_deliveries'
-- own denormalized fields.
CREATE TABLE disputes (
    id                        uuid        PRIMARY KEY,
    tenant_id                 uuid        NOT NULL,
    transaction_id            uuid        NOT NULL,
    status                    text        NOT NULL DEFAULT 'open'
                                          CHECK (status IN ('open','resolved_reversed','resolved_rejected')),
    reason                    text        NOT NULL,
    resolution_transaction_id uuid,
    created_at                timestamptz NOT NULL DEFAULT now(),
    resolved_at               timestamptz,
    -- Composite tenant FK (Task 5.4a pattern, migration 0023): the disputed
    -- transaction must belong to the SAME tenant as the dispute row itself.
    -- transactions carries UNIQUE (tenant_id, id) (migration 0001), so this
    -- is a valid FK target; a single-column FK on transaction_id alone would
    -- never check that tenant_id agrees, the same latent cross-tenant gap
    -- migration 0023 closed for idempotency_keys/audit_log/audit_outbox.
    CONSTRAINT disputes_txn_fk
        FOREIGN KEY (tenant_id, transaction_id) REFERENCES transactions (tenant_id, id)
);
-- The tenant's dispute list, newest first: the shape GET /v1/disputes pages by.
CREATE INDEX disputes_tenant_idx ON disputes (tenant_id, created_at DESC);

-- Row-level security, consistent with migration 0024/0027 (Task 5.4b, audit
-- A3.5): ENABLE + FORCE + one allow-when-unset tenant_isolation policy. See
-- migration 0024's own doc comment for the full reasoning (the "allow when
-- unset" branch keeps a trusted background worker's cross-tenant access
-- working; FORCE is what stops the owning role from bypassing its own
-- policies).
ALTER TABLE disputes ENABLE ROW LEVEL SECURITY;
ALTER TABLE disputes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON disputes
    USING (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (current_setting('app.tenant_id', true) IS NULL
           OR current_setting('app.tenant_id', true) = ''
           OR tenant_id::text = current_setting('app.tenant_id', true));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- NO FORCE before DISABLE (migration 0024's own down does the same, for the
-- same reason): relforcerowsecurity is a separate flag from relrowsecurity,
-- and DISABLE ROW LEVEL SECURITY alone does not clear it.
DROP POLICY tenant_isolation ON disputes;
ALTER TABLE disputes NO FORCE ROW LEVEL SECURITY;
ALTER TABLE disputes DISABLE ROW LEVEL SECURITY;

DROP INDEX disputes_tenant_idx;
DROP TABLE disputes;

-- +goose StatementEnd
