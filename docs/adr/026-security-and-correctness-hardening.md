# ADR-026: Security and correctness hardening from an external audit

Status: Accepted
Date: 2026-07-14

This ADR records a batch of hardening changes made in response to an external
audit of the codebase (a professional-grade review by a Go/fintech engineer).
The audit found the core double-entry invariant sound and unbreakable, but
flagged a set of authorization-integrity, safety, and correctness gaps. This
records the decisions behind fixing them. Nothing here changes the core
invariant: balances stay derived from an append-only posting history, enforced
per currency in the domain type and by the deferred database constraint trigger.

## Context

The audit rated the system "would not trust it with real money today" not
because money could be created or destroyed (it cannot) but because a few
controls did not deliver what they claimed, and a few reads and error paths
were not production-safe. The findings that drove code changes, and the
decisions taken, are below. The changes are deliberately backward-compatible
and default-off where a new guarantee needs an operator secret, so the demo
deployment and any existing clone behave exactly as before until the operator
opts in.

## Decisions

### 1. The audit actor is the API key, not the tenant

Every audit event and every held pending's `CreatedBy` stamped the tenant id as
the actor. That made two things impossible: attributing an action to an
individual principal, and enforcing maker-checker. The four-eyes flag
(`APPROVAL_REQUIRE_DIFFERENT_ACTOR`) compared tenant against tenant, so it was
either a no-op (flag off) or a self-deadlock that blocked every approval (flag
on), and a single admin key could create and approve the same over-threshold
movement. ADR-025 had claimed the control was real; the code did not deliver it.

Decision: thread the individual API-key id (`auth.PrincipalID`) as the acting
principal through the money paths, via a `ledger.WithActor` context value the
API and gRPC layers set from the resolved key. `holdForApproval` stamps the
creating key as `CreatedBy`; approve/reject/cancel record the deciding key; the
transaction and lifecycle audit events record the acting key. Background paths
(the pending sweep, the demo seeder) carry no principal and fall back to the
tenant id, unchanged. With this, `APPROVAL_REQUIRE_DIFFERENT_ACTOR` compares two
distinct keys and is a real maker-checker control. It stays off by default so
the demo (a single key that both creates and approves for demonstration) is
unchanged; a production deployment turns it on and issues separate maker and
checker keys.

We chose per-key rather than introducing a separate user/role model: the API
key is already the authenticated principal in this system, and a heavier
identity model is out of scope for v1.

### 2. Audit anchors are signed with an app-held key

`VerifyFromLatestAnchor` trusts the latest `audit_anchors` row as a checkpoint
and only re-walks the tail past it. But `audit_anchors` lives in the same
database a privileged attacker controls, so a consistent rewrite of `audit_log`
plus a matching rewrite of `audit_anchors` would pass verification. The
tamper-evidence claim was only as strong as an off-box log-shipping setup the
code did not enforce.

Decision: anchors carry an HMAC-SHA256 over `(tenant_id, chain_seq, row_hash)`
keyed by `AUDIT_ANCHOR_SIGNING_KEY`, a secret the database role does not hold
(migration 0037 adds the nullable `signature` column). The anchor job signs each
anchor; `VerifyFromLatestAnchor`, when a key is configured, requires a valid
signature before trusting the anchor. A forged `row_hash` the attacker cannot
re-sign is reported as not verifiable rather than trusted; an unsigned anchor
predating the key falls back to a full from-genesis verify. This closes the
in-database forgery path against a DB-privileged attacker. A full consistent
rewrite that also downgrades the anchor to unsigned is still ultimately caught
only by the off-box anchor copy, which remains the external ground truth; the
signature is defense in depth, not a replacement for it. Off by default and
unset in the demo, so the console's audit-verify panel is unchanged there.

### 3. FX rate staleness guard plus a free live feed

Conversions priced against whatever the latest stored rate was, with no maximum
age. With env-seeded v1 rates and no live feed, a long-running deployment would
happily convert real money against a weeks-old rate: a direct loss and
arbitrage vector.

Decision: add `FX_MAX_RATE_AGE`. A conversion whose resolved rate (direct,
inverted, or hub-composed) is older than this is refused with a stale-rate
error instead of pricing against it; a hub cross is dated by its older leg. The
guard is 0 (disabled) by default so nothing breaks silently. To keep the guard
from starving conversions without a paid data provider, add an optional feed
(`FX_FEED_ENABLED`) that polls a free, keyless, ECB-backed provider
(Frankfurter, daily) and appends fresh global rows, deduping against the current
stored mid so an unchanged rate does not append a row every interval. We chose
Frankfurter specifically because it needs no API key and no account, which suits
a portfolio-scale, open deployment; the provider URL is configurable so it can
be pointed elsewhere.

While here, a related pricing bug: a USD-hub cross conversion applied only the
base leg's spread, so a two-hop conversion was widened by one markup instead of
two, underpricing cross pairs against the equivalent pair of direct conversions.
Hub conversions now widen by both legs' spreads (summed, capped).

### 4. Webhook SSRF egress control and a signed timestamp

The webhook worker would POST to any address a tenant named, including internal
ones (`localhost`, cloud metadata, RFC1918), and followed redirects.

Decision: deliveries dial through a transport whose control hook rejects
loopback, link-local (including the metadata IP), private, unique-local, CGNAT,
unspecified, and multicast addresses. The check runs post-DNS on the concrete
IP, closing the DNS-rebinding window, and re-applies on every redirect hop since
each dials through the same transport. `WEBHOOK_ALLOW_PRIVATE_TARGETS` (default
false) re-enables private delivery for a demo or self-hosted deployment whose
receivers legitimately live on a private network. The signature now covers a
send timestamp (`X-Ledger-Timestamp`, signed as `"<ts>.<body>"`) so a subscriber
that bounds timestamp freshness is protected against replay of a captured
delivery.

### 5. Smaller correctness and safety fixes

- **Bounded reads.** The account tree and trial-balance endpoints built a
  response over every account with no limit. Both queries now cap at
  `MaxReportRows+1` and the service refuses an over-large result rather than
  building an unbounded response in memory. The cap is far above any
  demo/portfolio tenant, so the console is unchanged.
- **Idempotency double-post windows.** A request held as a pending under an
  idempotency key could post a second transaction if the approval gate was
  reconfigured off before the client retried; Post and Convert now dedup against
  an existing pending for the key. The separate post-TTL late-retry window is a
  standard bounded-idempotency tradeoff, documented with the per-tenant
  `reference` mitigation.
- **gRPC held-transaction mapping.** An over-threshold write over gRPC returned
  a generic `Internal` that looked like a server fault; it now maps to
  `FailedPrecondition` naming the pending id.
- **Fail-fast config and safe shutdown.** The server now refuses to boot on a
  set-but-unparseable env value (rather than silently substituting a default),
  the graceful-shutdown budget is configurable (`SHUTDOWN_TIMEOUT`), and a
  server error drains the servers before returning instead of racing the pool
  close.
- **CSV formula injection.** Export cells that are free text and begin with a
  formula trigger are neutralized so a spreadsheet renders them literally.

## Consequences

The controls the system advertises now hold: maker-checker is real when enabled,
the audit anchor is tamper-evident against a DB-privileged attacker when signed,
and conversions refuse stale rates when a max age is set. Every new guarantee is
opt-in behind an operator secret or flag, so the demo and existing deployments
are unchanged until configured. The audit's remaining items that were accepted
as fine for a demo deployment (the seeder's blast radius, the public demo admin
key, the absence of production Prometheus and offsite backups) are deliberately
not addressed here: they are operational choices of the demo box, documented in
the audit report, not defects in the service code.
