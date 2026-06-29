-- +goose Up
-- +goose StatementBegin

-- Tag the invariant triggers' exceptions with a constraint name so the
-- application can map them to typed errors (and clean HTTP 422 responses)
-- instead of a generic 500. Both raised check_violation (23514) before, which is
-- indistinguishable; now each carries CONSTRAINT = its trigger name.

CREATE OR REPLACE FUNCTION assert_txn_balanced() RETURNS trigger AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM postings
        WHERE transaction_id = NEW.transaction_id
        GROUP BY transaction_id
        HAVING SUM(amount) <> 0
    ) THEN
        RAISE EXCEPTION 'unbalanced transaction %: postings do not sum to zero', NEW.transaction_id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'postings_balanced';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION assert_posting_currency() RETURNS trigger AS $$
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
            USING ERRCODE = 'check_violation', CONSTRAINT = 'postings_currency_matches';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

CREATE OR REPLACE FUNCTION assert_txn_balanced() RETURNS trigger AS $$
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

CREATE OR REPLACE FUNCTION assert_posting_currency() RETURNS trigger AS $$
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

-- +goose StatementEnd
