-- +goose Up
-- +goose StatementBegin

-- Guarantee that every posting moves money in its account's own currency. The
-- domain enforces that all postings of a transaction share one currency, and the
-- transaction stores that currency, but nothing yet ties it to each account's
-- currency: without this, a USD transaction could post into a EUR account. This
-- trigger closes that gap at the database level.
--
-- It is a plain (immediate) AFTER INSERT trigger, not deferred: the account and
-- transaction both already exist when a posting is inserted (the foreign keys
-- require it), so the currencies can be checked right away. The reads are
-- equality lookups on primary keys, so they take only narrow predicate locks
-- under SERIALIZABLE.
CREATE FUNCTION assert_posting_currency() RETURNS trigger AS $$
DECLARE
    account_currency text;
    txn_currency     text;
BEGIN
    SELECT currency INTO account_currency
    FROM accounts
    WHERE tenant_id = NEW.tenant_id AND id = NEW.account_id;

    SELECT currency INTO txn_currency
    FROM transactions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.transaction_id;

    IF account_currency <> txn_currency THEN
        RAISE EXCEPTION 'posting currency mismatch: account % is % but transaction % is %',
            NEW.account_id, account_currency, NEW.transaction_id, txn_currency
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER postings_currency_matches
    AFTER INSERT ON postings
    FOR EACH ROW
    EXECUTE FUNCTION assert_posting_currency();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER postings_currency_matches ON postings;
DROP FUNCTION assert_posting_currency();
-- +goose StatementEnd
