-- +goose Up
-- +goose StatementBegin

-- Accounts are identity plus classification only. The balance is never stored
-- here; it is derived by summing postings (see ADR-001, ADR-003). Each account
-- belongs to exactly one tenant.
CREATE TABLE accounts (
    id         uuid        PRIMARY KEY,
    tenant_id  uuid        NOT NULL,
    name       text        NOT NULL CHECK (name <> ''),
    type       text        NOT NULL CHECK (type IN ('asset', 'liability', 'equity', 'income', 'expense')),
    currency   text        NOT NULL CHECK (char_length(currency) = 3 AND currency = upper(currency)),
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Target for the composite foreign key from postings: lets us enforce that a
    -- posting's account lives in the same tenant as the posting.
    UNIQUE (tenant_id, id)
);

-- A transaction is an atomic, immutable set of postings in a single currency.
-- The currency lives here, once, rather than on each posting: the domain already
-- enforces that all postings of a transaction share one currency
-- (Transaction.Validate), so a posting only needs to carry its signed amount.
CREATE TABLE transactions (
    id         uuid        PRIMARY KEY,
    tenant_id  uuid        NOT NULL,
    currency   text        NOT NULL CHECK (char_length(currency) = 3 AND currency = upper(currency)),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);

-- Postings are append-only signed entries. A positive amount is a debit, a
-- negative amount is a credit (see ADR-002). Balances are derived by summing
-- amount over an account. The sum-to-zero balance invariant is enforced in the
-- domain today (Transaction.Validate) and gets a database CHECK constraint in
-- Week 4; it is intentionally not present yet.
CREATE TABLE postings (
    id             uuid        PRIMARY KEY,
    tenant_id      uuid        NOT NULL,
    transaction_id uuid        NOT NULL,
    account_id     uuid        NOT NULL,
    amount         bigint      NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Composite foreign keys force a posting, its transaction, and its account to
    -- all share one tenant. A cross-tenant posting cannot be inserted.
    FOREIGN KEY (tenant_id, transaction_id) REFERENCES transactions (tenant_id, id),
    FOREIGN KEY (tenant_id, account_id)     REFERENCES accounts (tenant_id, id)
);

-- Balance reads sum amount per account within a tenant; this index serves them.
CREATE INDEX postings_tenant_account_idx ON postings (tenant_id, account_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE postings;
DROP TABLE transactions;
DROP TABLE accounts;
-- +goose StatementEnd
