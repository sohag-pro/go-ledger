-- +goose Up
-- +goose StatementBegin

-- Week 11 (multi-currency + FX, ADR-014): currency moves from the
-- transaction to the posting. A transaction used to carry one currency for
-- all its postings; an FX transaction needs two currencies in the same
-- transaction (the source leg and the converted leg), so a single
-- transaction-level currency column can no longer express every
-- transaction. This migration is ordered so a trigger never sees a NULL or
-- mixed grouping: add the column nullable, backfill it from each posting's
-- account, enforce NOT NULL, and only then swap the balanced and
-- currency-integrity triggers over.

-- 1. postings.currency, nullable first so existing rows do not fail the
--    ADD COLUMN.
ALTER TABLE postings ADD COLUMN currency text;

-- 2. Backfill from each posting's own account. Every posting written before
--    this migration was single-currency by construction (the old
--    posting-matches-transaction-currency trigger from migration 0003), and
--    an account's currency never changes after creation, so the account's
--    current currency is exactly the currency that posting was made in.
UPDATE postings p SET currency = a.currency
  FROM accounts a WHERE a.tenant_id = p.tenant_id AND a.id = p.account_id;

-- 3. Enforce, now that every row has a value.
ALTER TABLE postings ALTER COLUMN currency SET NOT NULL;
ALTER TABLE postings ADD CONSTRAINT postings_currency_len CHECK (char_length(currency) = 3);

-- 4. accounts.is_system marks the per-tenant per-currency FX clearing
--    accounts that the FX flow posts through. The clearing get-or-create is
--    an INSERT ... ON CONFLICT (tenant_id, name) WHERE is_system DO UPDATE,
--    so it needs a real conflict target. A partial unique index, rather
--    than a full UNIQUE on (tenant_id, name), is deliberate: existing user
--    accounts were never required to have unique names, so a full UNIQUE
--    constraint could abort this migration mid-deploy against real data.
--    Scoping the index to WHERE is_system only constrains the rows this
--    feature creates.
ALTER TABLE accounts ADD COLUMN is_system boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX accounts_system_name_uniq ON accounts (tenant_id, name) WHERE is_system;

-- 5. Replace the balanced trigger: the invariant is now "every currency
--    within the transaction sums to zero", not "the transaction sums to
--    zero". A transaction can legitimately hold two currencies for an FX
--    conversion, and a single sum across both would let one leg's surplus
--    mask the other leg's deficit. The trigger name, timing, and
--    ERRCODE/CONSTRAINT stay identical to migration 0005 so the
--    application's error mapping (internal/postgres/repository.go) still
--    resolves this to domain.ErrUnbalanced; only the query inside changes.
CREATE OR REPLACE FUNCTION assert_txn_balanced() RETURNS trigger AS $$
DECLARE
    bad_currency text;
BEGIN
    SELECT currency INTO bad_currency
    FROM postings
    WHERE transaction_id = NEW.transaction_id
    GROUP BY currency
    HAVING SUM(amount) <> 0
    LIMIT 1;

    IF bad_currency IS NOT NULL THEN
        RAISE EXCEPTION 'unbalanced transaction %: postings in % do not sum to zero', NEW.transaction_id, bad_currency
            USING ERRCODE = 'check_violation', CONSTRAINT = 'postings_balanced';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- 6. Replace the currency-integrity trigger: it now compares a posting's own
--    currency to its account's currency, not to a transaction-wide currency
--    (which no longer exists after step 7 drops it). Same name, timing,
--    ERRCODE, and CONSTRAINT as migration 0005, so it still resolves to
--    domain.ErrCurrencyMismatch.
CREATE OR REPLACE FUNCTION assert_posting_currency() RETURNS trigger AS $$
DECLARE
    account_currency text;
BEGIN
    SELECT currency INTO account_currency
    FROM accounts
    WHERE tenant_id = NEW.tenant_id AND id = NEW.account_id;

    IF NEW.currency <> account_currency THEN
        RAISE EXCEPTION 'posting currency mismatch: account % is % but posting % is %',
            NEW.account_id, account_currency, NEW.id, NEW.currency
            USING ERRCODE = 'check_violation', CONSTRAINT = 'postings_currency_matches';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- 7. transactions.currency no longer means anything: a transaction can now
--    span two currencies (an FX conversion's source and converted legs), so
--    there is no single value left to store there. Both trigger functions
--    above have already stopped reading it, so this drop is safe. Every
--    other consumer of this column (the sqlc transaction queries, the
--    REST/gRPC transaction DTOs, the audit payload, and the demo seeder) is
--    enumerated in the Task 3 report and fixed in later tasks this week.
ALTER TABLE transactions DROP COLUMN currency;

-- 8. fx_rates: an append-only table of quoted rates. mid_rate_e8 is the mid
--    rate scaled by 1e8 (fixed-point, matching the ledger's minor-unit
--    integer style; no floats anywhere in the invariant path). spread_bps is
--    applied on top of the mid rate when converting. base and quote must be
--    uppercase ISO 4217 codes and distinct from each other (a same-currency
--    "conversion" is not a conversion).
CREATE TABLE fx_rates (
    id           bigserial PRIMARY KEY,
    base         text NOT NULL CHECK (base = upper(base) AND char_length(base) = 3),
    quote        text NOT NULL CHECK (quote = upper(quote) AND char_length(quote) = 3),
    mid_rate_e8  bigint NOT NULL CHECK (mid_rate_e8 > 0),
    spread_bps   integer NOT NULL DEFAULT 0 CHECK (spread_bps >= 0 AND spread_bps < 10000),
    source       text NOT NULL,
    effective_at timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (base <> quote)
);

-- The "current rate" read is "the latest row for (base, quote) at or before
-- now"; id as a second descending key gives a deterministic tiebreaker when
-- two rows share an effective_at (for example a re-seed within the same
-- second), so "current" always resolves to one row, not an arbitrary one of
-- several.
CREATE INDEX fx_rates_current ON fx_rates (base, quote, effective_at DESC, id DESC);

-- 9. FX conversion snapshot on transactions: an immutable record of the rate
--    actually applied, so a transaction's converted amount stays
--    reproducible even after fx_rates accumulates newer quotes. All
--    nullable: a single-currency transaction (still the common case) has
--    none of this.
ALTER TABLE transactions
    ADD COLUMN fx_source_amount bigint,
    ADD COLUMN fx_converted_amount bigint,
    ADD COLUMN fx_mid_rate_e8 bigint,
    ADD COLUMN fx_spread_bps integer,
    ADD COLUMN fx_applied_e8 bigint,
    ADD COLUMN fx_rate_source text,
    ADD COLUMN fx_effective_at timestamptz,
    ADD COLUMN fx_rate_id bigint REFERENCES fx_rates(id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse order: drop what step 9 onward added, then restore what steps 5
-- to 7 replaced or removed. transactions.currency is re-added nullable, not
-- backfilled and not made NOT NULL: a transaction that came to span two
-- currencies under this migration has no single value to put there, and a
-- backfill or a NOT NULL constraint would fail on any such row.

ALTER TABLE transactions
    DROP COLUMN fx_rate_id,
    DROP COLUMN fx_effective_at,
    DROP COLUMN fx_rate_source,
    DROP COLUMN fx_applied_e8,
    DROP COLUMN fx_spread_bps,
    DROP COLUMN fx_mid_rate_e8,
    DROP COLUMN fx_converted_amount,
    DROP COLUMN fx_source_amount;

DROP INDEX fx_rates_current;
DROP TABLE fx_rates;

ALTER TABLE transactions ADD COLUMN currency text;

-- Restore the pre-0010 trigger bodies. assert_posting_currency reads
-- transactions.currency again, so it must be restored after the column
-- above is re-added, not before.
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

DROP INDEX accounts_system_name_uniq;
ALTER TABLE accounts DROP COLUMN is_system;

ALTER TABLE postings DROP CONSTRAINT postings_currency_len;
ALTER TABLE postings DROP COLUMN currency;

-- +goose StatementEnd
