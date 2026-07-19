# ADR-027: Webhooks

This ADR records go-ledger's webhook delivery: how a tenant subscribes to
ledger events, how deliveries are signed, and how they are delivered at least
once off the tamper-evident event stream. It is written in Week 14, the final
week, and it is deliberately retroactive: the webhook code shipped in an earlier
week without its own ADR, breaking the project's "an ADR before the code" rule.
Writing it now, after the fact, is the honest correction, and the gap itself is
one of the retrospective's lessons.

## Status

Accepted: 2026-07-14

## Context

The plan's Week 14 asked for "webhooks: per-tenant subscriptions, signed
delivery, retry with backoff, delivery log," consuming the Week 13 event stream.
By the time this feature was built, the pieces it needed already existed. The
audit chain (ADR-012, ADR-017) is an append-only, `chain_seq`-ordered,
tamper-evident log of every ledger event, produced by a single-leader background
chainer that drains the `audit_outbox` into `audit_log`. Approval lifecycle
events (ADR-025) already ride that same chain. So "event streaming" was not a
new system to stand up; it was a consumer to add.

The constraints a payment webhook has to satisfy shaped the design:

- A receiver must be able to verify a delivery came from the ledger and was not
  tampered with in flight, and must be able to reject a replayed capture.
- Delivery must survive a crash mid-send: a committed ledger event must
  eventually reach a healthy subscriber even if the process dies between the
  event and its delivery. That means at-least-once, and that means a receiver
  must be able to deduplicate.
- A slow or hostile subscriber URL must not be able to stall the ledger, exhaust
  it, or turn it into a proxy for reaching internal services.
- More than one app instance runs at a time, so exactly one of them must own the
  fan-out cursor, exactly as the chainer owns the chain.

## Decision

### 1. Fan out off `audit_log`, not the raw outbox

The webhook worker reads `audit_log` by `chain_seq`, the chained durable stream,
not `audit_outbox` (the pre-chain buffer). Delivering off the chained log means
webhooks see events in the same total order, and with the same durability, as
the tamper-evident record: a webhook is never sent for an event that is not yet
part of the verified chain. A per-worker fan-out cursor over `chain_seq` turns
each new chained event into one `webhook_deliveries` row per matching
subscription (migration 0021: `webhook_subscriptions`, `webhook_deliveries`).

### 2. Single leader, its own advisory lock

Running one worker per app instance is the intended shape. Like the chainer,
the worker takes a Postgres session advisory lock and only the holder runs a
fan-out-then-delivery pass; the rest wait. The webhook lock key (4_921_018) is
deliberately distinct from the chainer's (4_921_017): the two guard unrelated
resources (the hash chain vs. the fan-out cursor), so they must never contend on
one key. One leader means the fan-out cursor has a single writer and each event
fans out once, regardless of how many instances run.

### 3. Per-tenant subscriptions with an optional event filter

A `WebhookSubscription` is a tenant's registered callback: a URL, plus an
optional list of event types. An empty list means "every event"; a non-empty
list delivers only matching actions (for example `transaction.created`). The
signing secret is generated at creation, shown to the caller exactly once, and
never returned again by any read path (the subscription type carries no secret
field), so a leaked list response cannot reveal it.

### 4. Signed, timestamped deliveries; receiver dedup for at-least-once

Each delivery is a POST carrying:

- `X-Ledger-Signature: sha256=<hex HMAC>` over `"<unix-ts>.<body>"`, keyed by the
  subscription secret.
- `X-Ledger-Timestamp`: the Unix-seconds send time, part of the signed content,
  so a subscriber that bounds timestamp freshness rejects a replayed capture
  (added in the audit-remediation pass, ADR-026).
- `X-Ledger-Delivery-Id`: the delivery row id, stable across every retry of the
  same event, so a receiver deduplicates repeat deliveries by it. This is what
  makes at-least-once safe for the receiver: the ledger guarantees delivery, the
  receiver guarantees idempotency.
- `X-Ledger-Event`: the event type.

The body is re-marshaled from a fixed Go type on every attempt (Postgres does
not preserve a jsonb value's original bytes), so every retry of a row signs and
sends byte-identical content.

### 5. Retry with exponential backoff, then dead-letter

A delivery is `pending`, then on a 2xx it is `delivered`. Any other status or a
transport error schedules the next attempt at `now + backoff` and leaves it
`failed`; backoff is exponential from a 10s base, capped at 10 minutes. After
`MaxAttempts` (8) the row is marked `dead` and never retried. The
`webhook_deliveries` table is the delivery log: every attempt count, last error,
and terminal state is queryable, so an operator can see what a subscriber missed.

### 6. SSRF egress guard

The delivery client dials through a transport that refuses to connect to a
non-public address (loopback, link-local including the cloud metadata IP,
private, unique-local, CGNAT, unspecified, multicast). The check runs post-DNS
on the concrete IP, closing the DNS-rebinding window, and re-applies on every
redirect hop. `WEBHOOK_ALLOW_PRIVATE_TARGETS` (default false) re-enables private
delivery for a demo or self-hosted deployment whose receivers live on a private
network. This was added in the audit-remediation pass (ADR-026); without it a
tenant could name an internal URL and have the money service fetch it.

## Consequences

Webhooks reuse the event backbone the ledger already had, so there is no second
system to run (no Kafka, per the scope discipline): the same chained log that
proves the ledger's integrity is the stream subscribers consume. Delivery is
at-least-once, which pushes an idempotency requirement onto receivers, made
tractable by the stable delivery id. The trade the design does not make is
exactly-once or ordered-per-subscriber delivery: retries and the fan-out model
mean a subscriber can see a duplicate or an out-of-order delivery, and must
tolerate both. For a v1 that is the right cost: the alternative is per-subscriber
ordering state and a distributed dedup the receiver is better placed to own.
