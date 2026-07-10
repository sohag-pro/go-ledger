-- +goose Up
-- +goose StatementBegin

-- reverses_transaction_id links a reversal to the transaction it reverses
-- (Task 4.2, audit A1.2). Postings are append-only (ADR-001): a reversal is
-- never a mutation of the original, it is a brand new transaction whose
-- postings undo it, and this column is the only thing that ties the two
-- together. Nullable: an ordinary post has no reversal link at all.
ALTER TABLE transactions ADD COLUMN reverses_transaction_id uuid;

-- A reversal points at the transaction it reverses, in the same tenant: the
-- composite foreign key (not a bare reverses_transaction_id -> transactions.id)
-- is what makes a cross-tenant link structurally impossible, the same
-- tenant-scoping discipline every other foreign key in this schema follows.
ALTER TABLE transactions
    ADD CONSTRAINT transactions_reverses_fk
    FOREIGN KEY (tenant_id, reverses_transaction_id)
    REFERENCES transactions (tenant_id, id);

-- At most one reversal per original. This is the whole idempotency and
-- concurrency story for ReverseTransaction: the second attempt (whether a
-- deliberate retry or a genuine concurrent race) to insert a second reversal
-- row for the same original hits this unique violation, which the service
-- catches and turns into "read back the existing reversal" instead of a
-- second reversal ever landing. The partial WHERE clause means an ordinary,
-- non-reversal transaction (reverses_transaction_id IS NULL) is never
-- constrained by it.
CREATE UNIQUE INDEX transactions_one_reversal_idx
    ON transactions (tenant_id, reverses_transaction_id)
    WHERE reverses_transaction_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX transactions_one_reversal_idx;
ALTER TABLE transactions DROP CONSTRAINT transactions_reverses_fk;
ALTER TABLE transactions DROP COLUMN reverses_transaction_id;

-- +goose StatementEnd
