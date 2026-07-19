# ADR-025: Approval Workflows and a Lifecycle Event Stream

This ADR records adding an env-configured approval gate to go-ledger (Week 13:
approval workflows and event streaming). An over-threshold transaction is held
as pending and must be approved before it posts. Approval lifecycle transitions
emit events onto the existing tamper-evident chain, which the webhook fanout
already delivers. The core invariant is untouched: balances stay derived from
postings, and nothing an approver has not cleared ever reaches `postings`.

## Status

Accepted: 2026-07-13
Amended by ADR-026. The four-eyes control described in decision 3 shipped
non-functional; see the Amendment at the end of this ADR.

## Context

Every write path (post, convert, reverse) resolves its intended postings and
then, in one database transaction, writes `transactions` + `postings` and an
`audit_outbox` row that a background chainer later drains into the
tamper-evident `audit_log` (ADR-012, ADR-017). There is no notion of a
transaction that is authorized but not yet posted, and no control that holds a
large movement for a second pair of eyes.

Two things have to be true for an approval feature to fit this codebase. A
balance is never stored, only summed from an append-only posting history, so a
pending (unapproved) transaction must not create postings, or every balance
read would have to learn to exclude them. And the audit chain is the ledger's
integrity record: whatever carries approval events must not weaken the guarantee
that the transaction history verifies bit-for-bit.

The plan also asks for "event streaming." That backbone already exists: the
webhook fanout worker reads `audit_log` by `chain_seq` and delivers events to
subscribers. What is missing is events for the approval lifecycle, especially a
rejection, which never becomes a transaction and so has no transaction id to
hang off the existing transaction-scoped chain.

## Decision

### 1. The gate: largest leg per currency, env-configured, off by default

`APPROVAL_ENABLED` (default off) turns the feature on. `APPROVAL_THRESHOLDS` is
a per-currency map of minor-unit amounts (for example `USD:100000,EUR:90000`).
After screening and before persistence, a shared gate computes the largest
absolute posting amount per currency; if any currency's maximum exceeds its
configured threshold, the transaction is held. A currency with no configured
threshold is never gated. Judging per currency, on the single largest leg,
matches the per-currency zero-sum invariant: there is no cross-currency total to
compare against a single number.

### 2. Held as intent in a separate table, replayed on approval

A held transaction is written to a new `pending_transactions` table as its
intended request (the legs, reference, effective time, and any convert/reverse
parameters) in a `payload` JSON column, with `status='pending'`. Nothing touches
`postings` or `transactions`. On approval the normal `CreateTransaction(payload)`
path runs, so validation, the balance and currency triggers, and the ordinary
`transaction.created` audit event all happen in exactly one place. Balances
never reflect unapproved money, and there is no second posting path to keep
correct.

Re-validation happens at approval, against current balances, not at creation.
The pending stores intent; correctness is checked when it actually posts, so an
approval that would now overdraw fails and leaves the pending untouched rather
than posting stale, no-longer-valid money.

### 3. A dedicated `approve` scope, with an optional four-eyes flag

Authority is an api-key scope, not a new role. `approve` joins `read`, `post`,
and `admin`, with `admin` a superset as before. A separate scope keeps
"approve a large movement" distinct from "operate the tenant," which is the
point of the control. Self-approval is allowed by default so a single-key
deployment (the demo) works; `APPROVAL_REQUIRE_DIFFERENT_ACTOR` (default off)
enforces maker-checker (the approver's actor must differ from the creator's)
when a deployment wants it. Shipping the flag, off, keeps the four-eyes control
real and documented without breaking the demo.

### 4. One tamper-evident stream, extended to carry lifecycle events

Rather than add a second event outbox, the existing audit chain is extended to
carry non-transaction events. `audit_outbox` and `audit_log` gain a nullable
`transaction_id`, a `subject_type`/`subject_id` pair, and a `hash_version`
column. Existing transaction rows stay `hash_version=1` and hash exactly as
before; approval lifecycle events are `hash_version=2`, whose hash preimage
folds in the subject and tolerates a null transaction id. `ComputeAuditRowHash`
and chain verification branch on the row's version, so a chain mixing v1
transaction rows and v2 lifecycle rows verifies end to end, and every existing
row still recomputes to its stored hash.

This is the load-bearing risk of the week: the chain is the crown-jewel
integrity feature, and versioned hashing is a new way it could break. The trade
accepted here is one stream (one consumer, one fanout, one place to verify) in
exchange for mixing lifecycle events into the integrity record and taking on a
heavy verification-test burden (mixed-version chains, tampered v2 rows, and an
all-v1 regression guard on existing data).

### 5. Gated paths, with a reverse exemption

Post, convert, and reverse are all gated by the threshold. A reversal is
exempt in one case: when the transaction being reversed already cleared the gate
(it has a linked `pending_transactions` row with `status='approved'`). That
money was approved once; forcing its correction to wait for a second approval
would let a large erroneous posting trap the very reversal that fixes it. A
reversal of a directly-posted large transaction is still gated.

### 6. Full lifecycle: cancel, expiry, idempotent decisions

`pending` is the only non-terminal state. Beyond approve and reject, the creator
can cancel a pending (a distinct terminal state from an admin reject), and an
untouched pending expires after `PENDING_TTL` (default 72h) via a background
sweep that mirrors the idempotency sweep. Every decision takes a row lock, so
two racing decisions cannot both win; the loser sees a terminal state and gets a
409. Approving an already-approved pending returns the same transaction id
(idempotent). A gated create consumes its idempotency key against the pending,
so a replay returns the same pending rather than creating a second one.

## Events

All lifecycle events are v2 chain rows with
`subject_type='pending_transaction'` and `subject_id` the pending id, written to
`audit_outbox` in the same transaction as the state change and drained by the
existing chainer, exactly once per committed transition:

- `approval.requested` (gated create), `approval.approved` (also carries the
  posted transaction id), `approval.rejected`, `approval.cancelled`,
  `approval.expired`.

An approved over-threshold transaction produces two chain rows: the
`approval.approved` lifecycle event and the ordinary `transaction.created`
event. Webhooks fan these out with no new source; the payload builder learns to
render a subject-based (non-transaction) event.

## API surface

- Create paths keep their request shape; the response is 201 with the posted
  transaction when under threshold, or 202 with the pending resource when gated.
- `GET /v1/pending`, `GET /v1/pending/{id}` (read scope).
- `POST /v1/pending/{id}/approve` and `/reject` (approve scope; admin
  satisfies it).
- `POST /v1/pending/{id}/cancel` (creator only).
- The console gains a thin-client Approvals panel over these endpoints.

## Consequences

- A large movement is held for a second pair of eyes (or the same eyes, by
  configuration), then posts against current balances, with the whole lifecycle
  on the tamper-evident stream and out to webhooks.
- The core invariant is intact: no unapproved postings exist, balances stay
  derived, and the transaction chain still verifies. The feature is entirely
  opt-in (`APPROVAL_ENABLED=false` by default).
- The audit chain now carries non-transaction events. Every consumer that reads
  `audit_log` (verify, webhooks, the audit view) must tolerate a null
  transaction id and a subject reference. Chain verification is
  version-aware.
- The single largest risk is the versioned hash; it is mitigated by test
  coverage, not by design simplicity, and is called out as such.

## Alternatives considered

- **A separate `event_outbox` for lifecycle events.** Cleaner separation and
  lower risk to the integrity chain, at the cost of a second event source the
  webhook fanout would have to union. Rejected in favor of one stream, with the
  test burden accepted as the price.
- **Status column on `transactions` instead of a pending table.** Rejected: it
  puts unapproved postings in the table and forces every balance and report
  query to filter by status, reintroducing exactly the kind of stored,
  status-dependent state the ledger avoids.
- **Reuse the `admin` scope for approval.** Rejected: it conflates operating the
  tenant with authorizing money movement, and a dedicated scope is cheap.
- **Enforce four-eyes always.** Rejected as the default: it would make the
  single-key demo unable to approve its own held transactions. Kept as a flag.
- **Gate reversals unconditionally.** Rejected: it lets a large erroneous
  posting block the reversal that corrects it; a reversal of already-approved
  money is exempt.
- **Total-debits-per-currency or a single global threshold as the gate.**
  Rejected: gross-debit obscures which leg is large, and a single global number
  ignores that currencies differ in scale; largest-leg-per-currency is the
  clearest fit for the per-currency invariant.

## Amendment (ADR-026): the four-eyes flag shipped non-functional

Decision 3 above claims `APPROVAL_REQUIRE_DIFFERENT_ACTOR` "enforces
maker-checker (the approver's actor must differ from the creator's)." It did
not, and the external audit behind ADR-026 caught it.

The reason is one level of indirection. At the time, every audit event and every
held pending's `CreatedBy` stamped the **tenant id** as the actor, not the
individual key (a leftover from ADR-007, written before auth existed). So the
flag compared a tenant against itself. With the flag off it was a no-op, which
is what the demo exercised and why nothing looked wrong. With the flag on it
compared a value to itself and blocked every approval, including legitimate
ones: not a weak control, a self-deadlock. Either way a single admin key could
create and approve the same over-threshold movement, which is precisely the
thing the control exists to prevent.

ADR-026 fixed the underlying model rather than the comparison: the individual
API-key id (`auth.PrincipalID`) is now threaded through the money paths as the
acting principal, so `CreatedBy` and the deciding actor are distinct keys and
the flag compares two real principals. Background paths with no principal (the
pending sweep, the demo seeder) still fall back to the tenant id.

The lesson is recorded rather than edited away, because it is the more useful
artifact: this ADR asserted a control was real, the tests exercised it in the
configuration where it was a no-op, and nothing caught the gap until someone
read the code asking whether the claim held. A control that is documented,
flagged, and shipped is not the same as a control that works, and an ADR
claiming otherwise is exactly the kind of thing an audit is for.

## Out of scope (this week)

- Approval hierarchies or N-of-M sign-off; four-eyes is a single actor-difference
  flag.
- `account.created` as a chained event (the chain can now carry it; it is not
  wired this week).
- Per-tenant configurable thresholds (env-global only).
