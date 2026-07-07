# ADR-011: Testing strategy: unit, property, integration, chaos, load

## Status

Accepted: 2026-07-07

## Context

Through Week 8 the ledger accumulated tests package by package as each feature
landed: table-driven unit tests in `internal/domain`, testcontainers-backed
integration tests against real Postgres in `internal/postgres` and
`internal/ledger`, a stress test that throws concurrent transactions at the
service, and one property test on the balance invariant. What it did not have was
a strategy: a stated coverage bar, a way to enforce it, a load profile with a
number attached, or any test that asks what happens when Postgres dies in the
middle of a commit.

Week 9 is the testing week. The definition of done from the build plan is CI
green on a clean push and a load test that sustains 500 RPS at p99 under 100ms
locally. That is the visible target. The harder and more valuable goal is that
the test suite actually protects the money invariant under the conditions that
break payment systems in production: concurrency, partial failure, and retries
after an ambiguous outcome.

Coverage percentage is a weak proxy for that. Eighty percent line coverage on a
ledger can miss every branch that matters (rollback, overflow, currency mismatch,
double-post on retry) while padding the number with getters. So the decisions
here are less about hitting a percentage and more about which invariants get
tested and how the failure injection stays deterministic instead of flaky. A
flaky chaos test or a load test that secretly measures the wrong path is worse
than none: both ship a false "tested" signal, which is the last thing a payment
service wants.

## Decision

### A five-layer test pyramid, each layer with a distinct job

- **Unit** (fast, no Docker): domain types, money arithmetic, API and gRPC
  error mapping, paging cursors. Table-driven. These already dominate the suite
  and stay the default.
- **Property** (fast, no Docker, `pgregory.net/rapid`): invariants that must hold
  across a large input space, not just hand-picked cases. See below.
- **Integration** (Docker via testcontainers): the service and repository against
  real Postgres, so the schema, triggers, `CHECK` constraints, and SERIALIZABLE
  retry behaviour are exercised, not mocked.
- **Chaos** (Docker via testcontainers plus toxiproxy): partial-failure behaviour
  when the database connection dies mid-transaction. See below.
- **Load** (k6 against a Compose stack): throughput and tail latency under
  sustained traffic, in two contention profiles.

### Coverage: an 80% global floor, higher on the money path, generated code excluded

The gate is enforced by a `make cover` target and run in CI. It fails the build
below the floor rather than only reporting a badge, because a coverage number no
one blocks on is a number that silently rots.

The global floor on `internal/` is 80%. The money-critical packages carry higher
per-package floors, because that is where a missed branch costs real money:
`internal/domain` at 90%, `internal/ledger` at 85%, `internal/postgres` at 80%.
Transport and plumbing packages (`grpcserver`, `api`, `metrics`) count toward the
global floor but do not carry their own higher bar.

Generated code is excluded from the measurement: `internal/genproto` (protoc
output) and `internal/postgres/sqlc` (sqlc output). Testing generated code
measures the generator, not our work. The mechanics matter and are easy to get
wrong: the target emits a single merged coverage profile from one
`go test ./internal/... -coverprofile` run, strips the generated paths out of the
profile file, then reads the statement-weighted total from `go tool cover -func`.
Averaging per-package percentages would be a lie, because it weights a 20-line
package the same as a 2000-line one.

### Property tests target invariants, not just `Validate()`

The existing property test checks that `Transaction.Validate()` accepts a
balanced set and rejects an unbalanced one. That is the easy part. Three more
properties carry the real weight:

- **Money round-trip and bounds**: minor-unit `int64` amounts survive
  string-to-value-to-string round-trips, addition does not overflow silently, and
  zero and negative amounts are rejected where the domain forbids them. Money
  representation bugs are the classic ledger failure, so they get direct
  randomized coverage.
- **Stateful model check** (rapid state machine): a random sequence of
  transactions is applied, and after every step two invariants hold: each
  account balance equals the sum of its own postings, and the signed total across
  all accounts is zero. This catches an error that unit tests structurally cannot:
  a sequence of individually valid operations that corrupts the aggregate.
- **Concurrency invariant**: many goroutines post concurrently and the
  sum-of-postings invariant still holds with no lost updates, run under `-race`.

### Chaos uses toxiproxy for deterministic injection, and covers the ambiguous-commit case

Killing the Postgres container with `docker kill` at the right instant is timing
racy and slow: you usually land after the commit or before the write is in
flight, and then the test proves nothing while still occasionally going red on
CI. Instead a toxiproxy container sits between the app and Postgres, and the test
cuts the connection at a controlled point. That is repeatable, fast, and needs no
image restart.

Two distinct failures are tested, because they have different correct outcomes:

1. **Connection lost after `BEGIN`, before `COMMIT`**: the post must fail and roll
   back cleanly, leaving zero partial postings. The append-only invariant means a
   half-written transaction is not allowed to exist.
2. **Connection lost after `COMMIT` is sent but the acknowledgement is lost**: the
   client cannot tell whether the transaction committed. This is the scenario that
   loses money, because the natural client reaction is to retry. The test replays
   the same idempotency key and asserts exactly one transaction exists, not two.

This also pins down an invariant the integration tests take for granted: the
idempotency-key write and the transaction commit are one atomic database
transaction. If they were separate, a crash between them would either double-post
or wedge the key, and the ambiguous-commit test is what would catch a regression
that split them.

### Load: two contention profiles, unique keys, durability left on

The k6 script runs against a Compose stack (Postgres plus the app plus the
existing Jaeger) under a dedicated `load-test` profile. Two scenarios, because
they measure different ceilings:

- **Fan-out**: transactions spread across many accounts. This measures raw write
  throughput with minimal row-lock contention, the number the 500 RPS target is
  about.
- **Hot account**: many transactions against one account. This serializes on the
  per-account row lock and is where p99 blows up. The service already has a
  hot-account path; the load test exercises it on purpose rather than pretending
  contention does not happen.

Two rules keep the load test honest:

- **Every request carries a unique idempotency key**, with a small deliberate
  slice of duplicates mixed in. Without this the test would hammer the idempotency
  short-circuit (a cheap replay lookup) and report a throughput number for the
  wrong code path.
- **Durability stays on.** p99 on a laptop is dominated by fsync latency, and the
  tempting way to hit 100ms is to turn off `synchronous_commit`. On a ledger that
  is cheating: it trades the durability guarantee for a benchmark number. The
  target is reported as machine-dependent, measured with durability intact, not
  treated as a hard SLO.

The `load-test` Compose profile sets `SEED_ENABLED=false`. The demo seeder resets
the demo tenant every four hours and would collide with a load run, so the load
test uses its own tenant against a stack with the seeder off.

### CI: parallel test jobs, a non-gating smoke-load, deploy unchanged in intent

The existing `deploy.yml` runs one test-and-lint job before deploy. Week 9 splits
the pre-deploy work into jobs that run in parallel rather than chained, so a PR is
not paying for artificial serialization:

- `lint`: golangci-lint.
- `test`: the race-enabled suite plus the `make cover` gate. This one job runs the
  unit, property, integration, and chaos tests together, because the suite is not
  separated by build tags: the integration and chaos tests skip themselves when no
  Docker is present and run for real when it is. The GitHub runner has Docker, so
  the full suite runs and coverage reflects the real numbers. The testcontainers
  tests wait on the Postgres readiness log (`"ready to accept connections"` seen
  twice), not on the port opening, because port-open races with the initdb restart
  and produces connection resets on CI. Splitting unit from integration would mean
  either running the suite twice or inventing build tags the codebase does not use,
  so one honest `test` job is the better trade. The coverage profile is scoped to
  `internal/` only: `cmd/main.go` is wiring with no unit tests, and folding its zero
  into the number would drag the gate down for no signal, so `cmd/` is compiled and
  smoke-run as a build check but kept out of the coverage measurement.
- `smoke-load`: boots the Compose stack and runs a short low-RPS k6.

`deploy` needs `lint` and `test`, and still only runs on a push to `main`. PR test
runs get their own cancelable concurrency group so a force-push cancels the stale
run; the deploy concurrency group keeps `cancel-in-progress: false` so a release is
never interrupted mid-flight.

The smoke-load is deliberately not gated on the 500 RPS or 100ms figures, which
are laptop numbers and would flake on a shared runner. It is gated on the
regression signals that are stable across machines: a zero error rate and a loose
p99 ceiling (around 500ms). That catches a server that fell over or a ten-times
latency regression without going red because a CI runner was slow.

### Badges without a standing credential

The coverage badge is produced by writing a shields.io endpoint JSON to a
dedicated `badges` branch using the built-in `GITHUB_TOKEN`, and pointing shields
at the raw file. This avoids a long-lived personal access token with gist scope
sitting on a public repository: the built-in token is scoped to the run and needs
no manual secret. The Go Report Card badge is a plain URL to
`goreportcard.com`, which generates on first request and needs no setup.

## Consequences

### Positive

- Coverage is enforced, not decorative, with a higher bar where money moves and
  generated code excluded so the number reflects hand-written work.
- The invariant that defines the whole service is checked across a large random
  input space, including the aggregate-over-a-sequence case that example tests
  miss, and under concurrency with the race detector.
- The two failure modes that actually lose money in a payment system, a rollback
  after mid-transaction failure and a double-post after an ambiguous commit, have
  explicit deterministic tests rather than a hope that it works.
- The load test measures the real write path in both the throughput and the
  contention regime, with durability intact, so the number means something.
- CI stages run in parallel with a stable, non-flaky load gate, and the coverage
  badge needs no third-party account or standing token.

### Negative

- Two more infrastructure dependencies enter the test path: toxiproxy for chaos
  injection and a fuller Compose stack plus k6 for load. Both are Docker-gated and
  skip cleanly when Docker is absent, but they lengthen a full local run.
- Per-package coverage floors add friction: a change that drops
  `internal/ledger` below 85% fails CI even if the global number is fine. That is
  the intended pressure, but it is pressure.
- The load numbers are machine-dependent and not a portable SLO, so the DoD figure
  is a local measurement, not a guarantee that holds on a different box.
- toxiproxy is one more container image to pull and pin, and the ambiguous-commit
  test depends on injecting failure at a precise protocol point, which is more
  intricate than a plain integration test.

## Alternatives considered

- **`docker kill` for chaos injection**: rejected. Timing-racy and slow, it
  usually fires before the write or after the commit and proves nothing, while
  still flaking on CI. toxiproxy cuts the connection at a controlled point,
  repeatably and without an image restart.
- **Testing only the pre-commit failure**: rejected as incomplete. The
  ambiguous-commit case (commit sent, acknowledgement lost, client retries) is the
  one that double-posts, so it is the case worth writing.
- **A flat 80% coverage floor with no per-package bar**: rejected. It lets the
  money-critical packages sit at the floor while transport code carries the
  average. Higher floors on `domain`, `ledger`, and `postgres` put the pressure
  where a missed branch costs money.
- **Averaging per-package coverage percentages**: rejected as arithmetically
  wrong. It weights a tiny package equally with a large one. A single merged,
  statement-weighted profile is the honest number.
- **Load testing with a fixed idempotency key or a single account**: rejected.
  Fixed keys measure the replay short-circuit, and a single account measures only
  the contended path. Unique keys plus both a fan-out and a hot-account scenario
  measure the real write path and the real tail.
- **Relaxing `synchronous_commit` to hit the latency target**: rejected outright.
  It trades durability for a benchmark number, which is not a trade a ledger makes.
- **Gating the smoke-load on the 500 RPS / 100ms figures**: rejected. They are
  laptop numbers and would flake on shared CI. Gating on zero errors and a loose
  p99 ceiling catches real regressions without machine-dependent flakiness.
- **A gist-plus-PAT coverage badge (`schneegans/dynamic-badges-action`)**:
  rejected. It puts a long-lived gist-scoped personal access token on a public
  repo. Writing the badge JSON to a `badges` branch with the built-in
  `GITHUB_TOKEN` needs no standing secret.
- **Codecov for coverage**: not adopted. It adds an external SaaS dependency and a
  token for a portfolio repo whose whole ethos is self-hosted and dependency-light.
  A self-computed badge fits better.
