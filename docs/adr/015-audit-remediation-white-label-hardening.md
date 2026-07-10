# ADR-015: Audit remediation, white-label hardening, durability, and scale

## Status

Accepted: 2026-07-10

Basis: the fintech / white-label audit at `docs/audit/2026-07-10-fintech-white-label-audit.md`
(3 Blocker, 10 High, 15 Medium, 7 Low system findings; 6 must-fix and 7
nice-to-have book findings). This ADR records the remediation decision for each
area and the order the work lands in. Two of the Blockers (durability/DR and
multi-instance scaling) are deep enough that each gets its own detailed ADR
authored at the start of its phase; this ADR fixes the direction so the phasing is
committed.

## Context

The accounting core is sound: the zero-sum invariant is enforced in three layers,
money is integer minor units with no float, FX is single-step banker's-rounded
`big.Int` with immutable rate snapshots, idempotency is database-refereed, and the
audit log is hash-chained per tenant. The audit confirmed the ledger itself is not
the risk. The distance to "another company runs this for real money" is the shell:
no tenant entity, a daily same-disk `pg_dump` as the whole disaster-recovery story,
correctness pinned to a single instance by the per-tenant in-process mutex,
demo-shaped defaults, one latent FX money bug, an unhardened gRPC surface, and an
absent compliance / eventing / reporting layer. This ADR decides how each of those
is closed and in what order.

The goal is a production-grade, multi-tenant, white-label money core, and a premium
printable book. The work is phased by leverage and dependency, cheapest-highest-risk
first, so the disqualifying risks fall before the product features.

## Decision

### Phase 0: stop the bleeding (safe-by-default and the money bug)

**Safe-by-default deployment (A2.1).** Invert the demo-shaped defaults so a plain
deployment is production-safe. `SEED_ENABLED` defaults to `false`. The demo API key
is provisioned only when `DEMO_MODE=true` is set explicitly. The server refuses to
boot if `DEMO_API_KEY` equals the published public constant while `APP_ENV=production`.
The seeder refuses to run against any tenant that holds an API key other than the
demo key. The public key stays exactly what it is for go.sohag.pro (which sets
`DEMO_MODE=true`), and becomes impossible to enable by accident anywhere else.

**Currency minor-unit exponents (A1.1), the one latent money bug.** Introduce a
currency registry mapping each ISO code to its minor-unit exponent (USD 2, JPY 0,
BHD/KWD 3). `mid_rate_e8` is redefined explicitly as a major-unit ratio (quote per
base, both in major units), and `domain.Convert` applies the exponent factor
`10^(exp_quote - exp_base)` inside the integer computation so a USD to JPY
conversion is correct, not off by a power of ten. `Money.String()` formats to the
currency's real exponent. JPY and BHD test cases are added. This lands before any
non-2-decimal currency is ever configured.

### Phase 1: durability (the single disqualifying risk)

**Backup and disaster recovery (A4.1), Blocker.** A daily same-disk `pg_dump` is
not a money-grade recovery story. Decision: adopt continuous WAL archiving plus
periodic base backups shipped to encrypted offsite object storage, giving
point-in-time recovery, and add a scheduled automated restore-and-verify job that
restores into a throwaway environment and runs the balance invariant and
`audit/verify` walks over the restored data. Write RPO and RTO into the runbook.
Tool and topology (pgBackRest vs WAL-G; DB co-located with WAL shipping vs moving to
managed Postgres) are decided in the dedicated **ADR-017 (durability and DR)**
authored at the start of this phase. Default lean: pgBackRest to encrypted S3-
compatible storage, DB co-located for now, managed Postgres named as the growth
path. Nothing else in this remediation matters if this is not done.

### Phase 2: the tenant, the white-label MVP

**Tenant entity (A3.1) and key lifecycle (A3.2, A2.3), Blocker.** Introduce a
`tenants` table (id, name, status, settings jsonb, created_at) with a foreign key
from `api_keys`. Posting and reads gate on tenant status (active / suspended /
closed). Per-tenant settings hold what is global today: default currency, rate
limits, FX spread policy. API keys gain scopes (`read`, `post`, `admin`), optional
`expires_at`, and a `last_used_at` touch; the SHA-256-hashed, `glk_`-prefixed
storage stays. An operator-authenticated admin surface (an admin-scoped API, with a
thin CLI wrapper) provides: create tenant, issue key (plaintext shown once), rotate
with an overlap window, revoke, list. This replaces manual SQL and is the line
between a demo and a product. The fingerprint scheme id (A1.6) lands here too: store
a scheme version with each idempotency key so the next breaking fingerprint change
is free.

**Per-tenant FX and policy (A3.3, A3.4).** `fx_rates` gains a nullable `tenant_id`
(null = global default), resolved tenant-first in the provider; spread policy moves
into tenant settings. A per-tenant policy (max amount per transaction, daily volume,
currency allowlist) is enforced in the posting path.

### Phase 3: scale past one process

**Multi-instance audit-chain scaling (A3.6, A5.1), Blocker.** The per-tenant
in-process mutex is correct for one process and a documented cliff beyond it.
Decision: make the audit-chain extension asynchronous. A transaction commits its
postings without holding the chain; a single per-tenant chainer consumes an outbox
in commit order and appends audit rows, so the hot-path posting no longer serializes
on the chain read-then-append and multiple app instances stop fighting over it. The
detailed design (outbox schema, the chainer's leader-election / single-runner
guarantee, ordering, backpressure, and how `audit/verify` reads a possibly-lagging
chain) is recorded in the dedicated **ADR-018 (multi-instance audit chain)** at the
start of this phase. A two-instance same-tenant contention test (A8.2) is written
first so the current 40001 / 503 failure mode is measured, not guessed. Migrations
move to a CI step gated before the binary swap once instance count exceeds one
(A4.3).

### Phase 4: the integration surface (what makes integrators say yes)

- **Webhooks and eventing (A7.1).** Signed, retrying webhook delivery driven from
  the audit outbox (the same outbox as Phase 3), per-tenant subscriptions, a
  delivery log, at-least-once from the outbox. (This is the planned Week 13/14 work,
  pulled forward under the remediation.)
- **Reversal primitive (A1.2).** `POST /v1/transactions/{id}/reverse` posts the
  negated legs atomically, links both directions (`reverses_transaction_id`), is
  idempotent, and forbids reversing a reversal. The technical prerequisite for
  disputes.
- **External reference and value dating (A1.3).** Optional unique-per-tenant
  `reference` on transactions and an `effective_at` (value date) distinct from
  `created_at`; both are reconciliation prerequisites.
- **List, search, export (A7.2).** `GET /v1/transactions` with date-range and
  reference filters on the existing keyset pattern, plus CSV/JSON export.
- **Idempotency TTL (A1.4).** `expires_at` on idempotency keys (default 24h, config
  up to 7d), a sweep job, documented in the OpenAPI description.

### Phase 5: security depth and operations

- **gRPC parity (A2.2).** A rate-limit interceptor reusing `auth.Limiter`, a
  postings-count clamp to the same 100 as REST, an intentional `grpc.MaxRecvMsgSize`,
  and an explicit decision to either terminate TLS for gRPC or document it as a
  private-network-only surface.
- **Negative-lookup and edge throttling (A2.5).** nginx `limit_req` per IP, plus a
  small in-process per-IP negative-lookup throttle ahead of the auth DB lookup, and
  trust `X-Forwarded-For` from localhost only (A6.4).
- **Audit-chain anchoring and streaming verify (A2.4).** Periodically anchor each
  tenant's head `row_hash` off-box (a shipped log line or object-store write) so a
  privileged DB rewrite is detectable; make `audit/verify` stream/page with a stored
  checkpoint instead of loading the whole chain.
- **Composite tenant FKs (A4.4).** `idempotency_keys` and `audit_log` reference
  `transactions(tenant_id, id)` compositely, matching `postings`.
- **RLS defense in depth (A3.5).** Postgres row-level security keyed on a per-
  transaction GUC (`SET LOCAL app.tenant_id`), so a future missed `WHERE tenant_id`
  cannot leak across tenants.
- **Account state and balance policy (A1.5).** Account `status`
  (active/frozen/closed) and an optional `min_balance` enforced in the SERIALIZABLE,
  per-tenant-serialized posting path; the first compliance-hold and overdraft
  control.
- **Alerting and SLOs (A6.1).** Commit `deploy/alerts.yml`: balance-invariant
  canary, serialization-retry rate, 5xx rate, p99 post latency, scheduled
  `audit/verify`, cert expiry, disk space, backup success/age. Add a dashboard, an
  uptime monitor, a paging channel, and written SLOs.
- **Deploy safety (A6.2), off-box logs (A6.3), secrets and TLS (A2.6).** Automated
  rollback on failed health check, a build-SHA field in `/healthz` so the gate
  confirms the new binary, off-box journald shipping with a retention policy,
  encrypted backups, a secrets story, and `sslmode=verify-full` when the DB moves
  off-box.

### Phase 6: compliance scaffolding

Enough hooks that a compliance stack bolts on without forking the core: account
freeze status (from A1.5), a pre-post policy hook interface where a screening /
monitoring decision can gate a post, party / customer reference fields on accounts,
a PII and retention policy that keeps money data immutable while making description
fields crypto-shreddable (resolving the immutability-vs-erasure tension), and the
statement/reporting/dispute data model (statement documents, per-tenant trial
balance and per-currency totals, dispute records built on the reversal primitive).
Much of this is the planned Week 12 reporting work, named here against the
white-label bar.

### Phase 7: the premium book pass (interleaved, cheap wins pulled earlier)

- **B1 colophon drift (must-fix).** The edition and support pages say "Weeks 1 to
  10, ADRs 001 to 013, Weeks 11 to 14 coming soon" while the book contains Week 11
  and ADR-014. Derive those lines from the build manifest so they can never drift
  again. (This is cheap and pulled forward to Phase 0.)
- **B2 back-matter numbering (must-fix).** Further Reading renders as sections
  40.7+; switch the back matter to unnumbered sectioning.
- **B3 print production spec (must-fix).** Decide the physical product: a real trim
  (170x240 mm or 6x9in, not A4), add bleed to the full-bleed dark pages, produce a
  separate cover file with computed spine width; until then label the artifact
  "digital edition."
- **B4 grayscale (must-fix).** Add redundant, non-hue encoding to the zero-sum motif
  and the yes/no and pass/reject diagrams, then do a full grayscale proof pass.
- **B5 figure placement (must-fix).** Audit every colon-introduced figure so the
  deictic "here is the picture" points at the picture (non-floating placement).
- **B6 index quality (must-fix).** Rebuild the index with subentries and bolded
  defining pages instead of 30-locator concordance runs; consider retitling to the
  conventional "Index."
- **Nice-to-have (B7 to B13).** ISBN and edition apparatus, tagged-PDF / EPUB
  accessibility with diagram alt text, split or number the two special editions,
  captioned/numbered listings with a List of Listings, `epubcheck` in CI, a
  typographic proof sweep, and explicit early-access completeness framing.

## Consequences

- The disqualifying risks (durability, safe defaults, the FX money bug) fall first,
  in Phases 0 and 1, so the system is safe to run for real money early, before the
  product features are complete.
- Introducing a `tenants` table and, later, RLS and composite FKs are schema
  migrations touching the hot tables; each is an ordered, tested migration in the
  style of 0010, with the multi-instance migration-on-boot change (A4.3) landing
  alongside Phase 3.
- The outbox-chainer (Phase 3) is the deepest change: it moves the audit chain off
  the synchronous posting path, which changes `audit/verify` semantics (the chain
  may briefly lag a committed transaction) and requires a single-runner guarantee.
  ADR-018 records that design; it is not hand-waved here.
- Per-finding severity and file references stay in the audit report; this ADR is the
  decision and sequencing record, and the implementation plan
  (`docs/superpowers/plans/2026-07-10-audit-remediation.md`) maps every finding to a
  task within these phases.
- Two Blockers (DR, multi-instance) and the largest features (webhooks, compliance)
  are multi-week; this ADR commits the direction and order, not a promise that all
  of it lands in one week. The cheap, high-leverage fixes (Phase 0, the book
  must-fixes, gRPC parity, composite FKs, reversal, refs) can land quickly; the
  Blockers are scheduled, not skipped.

## Alternatives considered

- **Advisory-lock or per-partition sharding for multi-instance (A3.6):** rejected as
  the primary path in favor of the outbox-chainer, because a database advisory lock
  under SERIALIZABLE was already shown to fail (ADR-012), and sharding the chain
  complicates verification. Recorded fully in ADR-018.
- **Managed Postgres now (A4.1/A4.2):** the cleaner durability answer, deferred as
  the growth path rather than the immediate step, because WAL archiving off the
  current box removes the disqualifying data-loss risk at far lower cost and change;
  ADR-017 decides the trigger for the move.
- **Per-tenant RLS as the only isolation:** rejected as a replacement; adopted as
  defense in depth on top of the existing composite-FK and tenant-scoped-query
  isolation, which stays the primary guarantee.
- **Fixing everything in one pass:** rejected as dishonest scheduling. The work is
  phased; the Blockers are named, ordered, and given their own detailed ADRs at the
  point they are built.

## Out of scope (even for this remediation)

Obtaining a money-transmitter license or partner-bank sponsorship (a business, not
an engineering, task), building the compliance decision engine itself (only the
hooks), Kafka or an external broker (the outbox covers eventing), event sourcing,
and a product web UI beyond the developer console.
