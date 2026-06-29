-- +goose Up
-- +goose StatementBegin

-- The balance trigger below reads postings by transaction_id at COMMIT. Without
-- an index that read is a scan, which under SERIALIZABLE (SSI) takes broad
-- predicate locks and causes a storm of false-positive serialization conflicts
-- under concurrency. A precise index keeps the trigger's read narrow. It also
-- serves ListPostingsByTransaction.
CREATE INDEX postings_transaction_idx ON postings (transaction_id);

-- Enforce the double-entry balance invariant at the database level: every
-- transaction's postings must sum to zero. A row-level CHECK cannot express this
-- because the sum spans many posting rows, so we use a constraint trigger marked
-- DEFERRABLE INITIALLY DEFERRED. It is queued and fires once at COMMIT, after all
-- of a transaction's postings are in, no matter how many statements inserted them
-- or which client wrote them. This is defense in depth: the domain
-- (Transaction.Validate) already checks the invariant, but this guarantees it
-- holds even against a buggy caller, a bad migration, or a direct psql write.
CREATE FUNCTION assert_txn_balanced() RETURNS trigger AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM postings
        WHERE transaction_id = NEW.transaction_id
        GROUP BY transaction_id
        HAVING SUM(amount) <> 0
    ) THEN
        RAISE EXCEPTION 'unbalanced transaction %: postings do not sum to zero', NEW.transaction_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Constraint triggers must be FOR EACH ROW, so for an N-leg transaction this
-- fires N times at COMMIT and runs the same per-transaction SUM check each time.
-- N is tiny (a transaction is usually two legs), so the repeated work is
-- negligible; this is intentional, not an oversight to "optimize" into a
-- per-statement trigger (which cannot be DEFERRABLE).
CREATE CONSTRAINT TRIGGER postings_balanced
    AFTER INSERT ON postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION assert_txn_balanced();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER postings_balanced ON postings;
DROP FUNCTION assert_txn_balanced();
DROP INDEX postings_transaction_idx;
-- +goose StatementEnd
