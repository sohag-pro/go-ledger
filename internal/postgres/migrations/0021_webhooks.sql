-- +goose Up
-- +goose StatementBegin

-- Task 4.1 (audit A7.1): signed, retrying, at-least-once webhook delivery to
-- per-tenant subscribers, fanned out off the durable, chain_seq-ordered
-- audit_log stream built in Phase 3 (ADR-017), not off audit_outbox (which
-- the chainer alone consumes and marks).

-- webhook_subscriptions: one row per tenant-configured callback URL. secret
-- is the HMAC-SHA256 signing key (Task 4.1): unlike an api_keys row, it is
-- stored as-is, not hashed, because it must be read back to sign every
-- outbound payload; it is generated CSPRNG (domain.GenerateWebhookSecret)
-- and returned to the caller exactly once, at creation time, the same
-- once-only discipline api_keys plaintext already follows.
-- event_types is the subscriber's filter: an empty array means "every
-- action", so a brand-new subscription with no filter configured receives
-- everything rather than nothing.
CREATE TABLE webhook_subscriptions (
    id          uuid        PRIMARY KEY,
    tenant_id   uuid        NOT NULL REFERENCES tenants (id),
    url         text        NOT NULL,
    secret      text        NOT NULL,
    event_types text[]      NOT NULL DEFAULT '{}',
    active      boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_subscriptions_tenant_idx ON webhook_subscriptions (tenant_id) WHERE active;

-- webhook_deliveries: one row per (subscription, audit event) fan-out. The
-- UNIQUE (subscription_id, audit_chain_seq) constraint is what makes fan-out
-- exactly-once into this table even if the fan-out step ever ran twice (a
-- restarted worker replaying a batch it had already inserted, or, in the
-- pathological case ADR-017's own outbox_id backstop guards against, two
-- workers briefly believing themselves leader): a second attempt to insert
-- the same (subscription, event) pair is a no-op conflict, not a duplicate
-- delivery row. Delivery itself is at-least-once, not exactly-once: a
-- receiver dedups repeat deliveries of the same event using the delivery id
-- (this row's own id, sent as X-Ledger-Delivery-Id), which is stable across
-- every retry of the same row.
CREATE TABLE webhook_deliveries (
    id              uuid        PRIMARY KEY,
    tenant_id       uuid        NOT NULL,
    subscription_id uuid        NOT NULL REFERENCES webhook_subscriptions (id),
    audit_chain_seq bigint      NOT NULL,
    event_type      text        NOT NULL,
    payload         jsonb       NOT NULL,
    status          text        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending','delivered','failed','dead')),
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    delivered_at    timestamptz,
    UNIQUE (subscription_id, audit_chain_seq)
);
-- The delivery worker's due-work scan: pending/failed rows whose backoff has
-- elapsed, oldest due first. delivered and dead rows are never scanned
-- again, so the partial index stays small regardless of how much delivered
-- history accumulates.
CREATE INDEX webhook_deliveries_due_idx
    ON webhook_deliveries (next_attempt_at) WHERE status IN ('pending','failed');

-- webhook_fanout_cursor: a singleton row recording the last audit_log.chain_seq
-- the fan-out step has scanned (whether or not it produced any delivery
-- rows, since a chained event with no matching subscription still advances
-- the cursor past it). The CHECK (id) constraint, the same "boolean primary
-- key that can only ever be true" trick used nowhere else yet in this
-- schema, keeps the table physically incapable of holding a second row: a
-- second INSERT would either violate the PRIMARY KEY (id already true) or
-- fail the CHECK (id was somehow false), so there is exactly one cursor,
-- structurally, not just by convention.
CREATE TABLE webhook_fanout_cursor (
    id             boolean PRIMARY KEY DEFAULT true CHECK (id),
    last_chain_seq bigint  NOT NULL DEFAULT 0
);
INSERT INTO webhook_fanout_cursor (id, last_chain_seq) VALUES (true, 0) ON CONFLICT DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE webhook_fanout_cursor;
DROP INDEX webhook_deliveries_due_idx;
DROP TABLE webhook_deliveries;
DROP INDEX webhook_subscriptions_tenant_idx;
DROP TABLE webhook_subscriptions;

-- +goose StatementEnd
