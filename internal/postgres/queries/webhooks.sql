-- name: InsertWebhookSubscription :exec
-- Inserts a new webhook_subscriptions row, always active (a caller creates a
-- subscription to receive events; deactivating happens later, via
-- SetWebhookSubscriptionActive). secret is stored as-is, never hashed (Task
-- 4.1, audit A7.1): the delivery worker must read it back in full to sign
-- every outbound payload.
INSERT INTO webhook_subscriptions (id, tenant_id, url, secret, event_types)
VALUES ($1, $2, $3, $4, $5);

-- name: ListWebhookSubscriptionsByTenant :many
-- Every subscription for a tenant, oldest first, active or not: the admin
-- surface's list view. Never selects secret: it is shown once, at creation
-- time, and never recoverable through a list call.
SELECT id, tenant_id, url, event_types, active, created_at
FROM webhook_subscriptions
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY created_at, id;

-- name: SetWebhookSubscriptionActive :execrows
-- Flips active for one subscription by id. The admin surface's
-- DeleteSubscription calls this with active=false instead of a hard DELETE
-- (see domain.Repository.SetWebhookSubscriptionActive's doc comment for
-- why): a webhook_deliveries row's foreign key to its subscription has no
-- cascade, so deactivating, not deleting, is what "stops future deliveries"
-- without breaking or discarding delivery history.
UPDATE webhook_subscriptions SET active = sqlc.arg(active) WHERE id = sqlc.arg(id);

-- name: ListActiveWebhookSubscriptionsByTenant :many
-- The fan-out step's per-tenant read (Task 4.1): every ACTIVE subscription
-- for one tenant, including the secret and event_types the fan-out needs to
-- decide whether and how to create a webhook_deliveries row for it. Ordered
-- by id only for a stable, deterministic iteration order within one fan-out
-- pass; it carries no meaning beyond that.
SELECT id, tenant_id, url, secret, event_types
FROM webhook_subscriptions
WHERE tenant_id = sqlc.arg(tenant_id) AND active
ORDER BY id;

-- name: GetWebhookFanoutCursorForUpdate :one
-- Reads the singleton fan-out cursor and locks its row for the rest of the
-- surrounding transaction (Task 4.1): the same "read the watermark, then
-- act, all in one transaction" shape ADR-017's chainer uses for audit_log,
-- so the cursor read and its eventual advance (SetWebhookFanoutCursor) never
-- race a concurrent fan-out pass.
SELECT last_chain_seq FROM webhook_fanout_cursor FOR UPDATE;

-- name: SetWebhookFanoutCursor :exec
-- Advances the singleton fan-out cursor. Always called in the same
-- transaction as the webhook_deliveries inserts it corresponds to (Task
-- 4.1), so a crash between the two is impossible: either both the inserts
-- and the cursor advance land, or neither does, and a retried fan-out pass
-- sees the same audit_log rows again (harmless: InsertWebhookDelivery's
-- ON CONFLICT DO NOTHING against the (subscription_id, audit_chain_seq)
-- unique index makes re-inserting the same pairing a no-op).
UPDATE webhook_fanout_cursor SET last_chain_seq = sqlc.arg(last_chain_seq);

-- name: ListAuditLogSinceChainSeq :many
-- The fan-out step's source read: every chained audit_log event past the
-- cursor, oldest first, up to batch_limit. Reads chain_seq (ADR-017's
-- failover-safe, skew-proof linearization key), not id or created_at, for
-- the same reason GetLastAuditHash does: it is the one order the chainer
-- itself guarantees is monotonic across any failover.
--
-- transaction_id is nullable (ADR-025, migration 0034): a chained
-- non-transaction lifecycle event (for example approval.rejected) has none.
-- The fan-out worker maps a null transaction_id to an empty, omitted field
-- in the webhook payload, the same convention the rest of the audit read
-- path uses. subject_type/subject_id (also ADR-025) are what that kind of
-- event carries instead: the fan-out worker copies them onto the payload
-- (Task 10) so a consumer can tell which subject the event concerns.
SELECT chain_seq, tenant_id, action, transaction_id, subject_type, subject_id, after, created_at
FROM audit_log
WHERE chain_seq > sqlc.arg(after_seq)
ORDER BY chain_seq
LIMIT sqlc.arg(batch_limit);

-- name: InsertWebhookDelivery :execrows
-- Creates one fan-out row for a (subscription, audit event) pairing. The
-- ON CONFLICT DO NOTHING against the UNIQUE (subscription_id,
-- audit_chain_seq) index (migration 0021) is what makes fan-out
-- exactly-once into this table even if the fan-out step ever ran twice over
-- the same audit_log range: a repeat attempt is a silent no-op, not a
-- duplicate delivery row, so execrows reports 0 for a pairing that already
-- existed and 1 for a genuinely new one.
INSERT INTO webhook_deliveries (id, tenant_id, subscription_id, audit_chain_seq, event_type, payload)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (subscription_id, audit_chain_seq) DO NOTHING;

-- name: ListDueWebhookDeliveries :many
-- The delivery worker's batch read: pending/failed rows whose backoff has
-- elapsed, oldest due first, joined to their subscription for the url and
-- secret needed to sign and send. "AND ws.active" means a subscription that
-- was deactivated after fan-out already created pending rows for it simply
-- stops being picked up here (see SetWebhookSubscriptionActive's doc
-- comment): its existing rows are left exactly as they are, neither
-- delivered nor purged, just never attempted again. FOR UPDATE OF wd SKIP
-- LOCKED is defense in depth against two workers ever running at once (the
-- leader-election lock is what actually prevents that in normal operation,
-- ADR-017's discipline mirrored for this worker): a second reader skips a
-- row the first is mid-processing rather than picking it up concurrently.
-- This query runs as its own standalone statement (never inside a
-- surrounding transaction that also makes the outbound HTTP call), so the
-- row lock is released the moment this statement completes; correctness
-- does not depend on holding it any longer than that.
SELECT wd.id, wd.tenant_id, wd.subscription_id, wd.event_type, wd.payload, wd.attempts,
       ws.url, ws.secret
FROM webhook_deliveries wd
JOIN webhook_subscriptions ws ON ws.id = wd.subscription_id
WHERE wd.status IN ('pending', 'failed') AND wd.next_attempt_at <= now() AND ws.active
ORDER BY wd.next_attempt_at
LIMIT sqlc.arg(batch_limit)
FOR UPDATE OF wd SKIP LOCKED;

-- name: MarkWebhookDeliveryDelivered :execrows
-- A 2xx response: terminal success. attempts is set to the total number of
-- tries this delivery took (including the successful one), the same total
-- MarkWebhookDeliveryFailed accumulates on the way here, so attempts always
-- means "how many times this delivery was actually tried", not just "how
-- many times it failed". "AND status IN ('pending', 'failed')" guards
-- against a state regression: in a two-leader window (SKIP LOCKED is
-- defense in depth, not a guarantee, see ListDueWebhookDeliveries) two
-- workers can pick up the same due row, and without this guard a slower
-- worker's write could still land after a faster worker already reached a
-- terminal outcome for it. Once a row is 'delivered' or 'dead', neither mark
-- query can move it again: this returns 0 affected rows instead, which the
-- caller (internal/webhook) treats as a no-op, not an error.
UPDATE webhook_deliveries SET status = 'delivered', delivered_at = now(), attempts = sqlc.arg(attempts)
WHERE id = sqlc.arg(id) AND status IN ('pending', 'failed');

-- name: MarkWebhookDeliveryFailed :execrows
-- A non-2xx response or transport error: the caller (internal/webhook)
-- computes the new attempts count, the resulting status ('failed' to retry
-- again later, or 'dead' once attempts reaches the configured max), the next
-- backoff deadline, and the error text to record, all in Go (backoff and the
-- max-attempts cap are application config, not schema), and this query just
-- persists that decision. "AND status IN ('pending', 'failed')" is the same
-- terminal-state guard MarkWebhookDeliveryDelivered carries (see its doc
-- comment): a delivery already 'delivered' (or already 'dead') can never be
-- regressed by a late failure write from a second worker that raced this one.
UPDATE webhook_deliveries
SET status = sqlc.arg(status), attempts = sqlc.arg(attempts), next_attempt_at = sqlc.arg(next_attempt_at), last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND status IN ('pending', 'failed');

-- name: GetWebhookDelivery :one
-- Raw fetch by id: tests and any future delivery-inspection tooling read a
-- single row's full lifecycle state back this way.
SELECT id, tenant_id, subscription_id, audit_chain_seq, event_type, payload, status, attempts, next_attempt_at, last_error, created_at, delivered_at
FROM webhook_deliveries
WHERE id = sqlc.arg(id);

-- name: ListWebhookDeliveriesBySubscription :many
-- Every delivery row for one subscription, in fan-out order (the same
-- chain_seq order the source audit_log events were read in). Used by tests
-- to assert fan-out and delivery outcomes end to end.
SELECT id, tenant_id, subscription_id, audit_chain_seq, event_type, payload, status, attempts, next_attempt_at, last_error, created_at, delivered_at
FROM webhook_deliveries
WHERE subscription_id = sqlc.arg(subscription_id)
ORDER BY audit_chain_seq;
