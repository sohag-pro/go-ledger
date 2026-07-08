# Load test

`test/load/post_transactions.js` is a k6 script that drives POST /v1/transactions
against a running go-ledger instance. It has no remote dependencies: it does
not import any jslib.k6.io helper, so it builds and runs offline.

## How to run

Bring up the dedicated load-test stack (real Postgres, no demo seeder, its own
tenant) and run the script against it:

```sh
make load
```

`make load` brings the stack up with
`docker compose --profile load-test up -d --build` and waits for `/healthz`,
then you run k6 yourself, for example:

```sh
k6 run test/load/post_transactions.js
```

Tear the stack down afterward:

```sh
docker compose --profile load-test down
```

Override the target with `BASE_URL` if you are pointing at something other
than `http://localhost:8080`:

```sh
BASE_URL=http://localhost:9090 k6 run test/load/post_transactions.js
```

### CI smoke profile

CI (and anyone checking the script still works without waiting on a full run)
sets `SMOKE=1`, which shrinks the run to 30 seconds at 50 requests per second,
a single scenario, and thresholds that only check for zero failed requests
and a loose p99 ceiling:

```sh
SMOKE=1 k6 run test/load/post_transactions.js
```

## What it does

Each tenant's audit hash chain serializes same-tenant transaction posts
through an in-process mutex, so a single tenant's throughput is bounded no
matter how high its rate limit is. To reach 500 requests per second in
aggregate, this script spreads traffic across `LOAD_TENANTS` tenants
(default 8, matching the load-test Compose stack's `LOAD_TEST_TENANTS`),
each with its own derived key `<API_KEY>-t<i>`.

`setup()` runs once before any traffic starts. For each tenant it creates 20
real USD asset accounts through POST /v1/accounts plus one extra dedicated
account for the hot_account scenario, all authenticated with that tenant's
key, and passes the real UUIDs (grouped per tenant) into the scenario
functions. The script never invents account ids: every posting's account_id
has to reference an account that actually exists, belongs to the caller's
tenant, and matches the transaction's currency, so setup has to create them
first.

Every POST /v1/transactions carries a unique `Idempotency-Key` header built
from the VU id, iteration counter, a random suffix, and the clock, so keys
are unique across VUs and iterations. A small fraction of requests (about 5
percent) deliberately reuse a prior key together with its original request
body and tenant, to exercise the replay path (the server answers with the
same transaction and an `Idempotent-Replayed: true` response header, still
with status 201).

### Scenarios

- **fanout**: ramps up over 15 seconds and then holds at 500 requests per
  second for 45 seconds (1 minute total). Each request first picks a random
  tenant, then picks two different accounts at random out of that tenant's
  20 accounts created in setup, so writes and their row locks are spread
  across many tenants and, within each tenant, many accounts.
- **hot_account**: runs for 1 minute right after fanout, holding a steady 200
  requests per second. Each request picks a random tenant, then uses that
  tenant's dedicated hot account on one leg and a random account from that
  same tenant's pool on the other leg, so writes contend for one account's
  balance and row lock within a tenant, while the contention itself is
  spread across tenants.

Both scenarios post a balanced two-leg transaction (`amount` and `-amount`,
signed minor units), matching the domain's double-entry invariant.

## Durability

The load-test stack does not touch `synchronous_commit`. Durability stays on
for these runs, the same as production. The point of this script is to
measure throughput and latency under real fsync behavior, not to see what the
service looks like with durability relaxed.

## About the 500 RPS / p99 under 100ms figure

The `fanout` scenario is configured to ramp to and hold 500 requests per
second, with a threshold reporting whether p99 latency stays under 100
milliseconds during that scenario. This threshold is not an SLO and it does
not abort the run if it is missed: it is a local, machine dependent
measurement, taken on whatever laptop or CI runner happens to execute it, in
a plain Docker Compose stack next to that runner's other work. Treat the
number as a rough evidence point, not a spec.

On a local Apple Silicon development machine with the load-test Compose stack
(app plus Postgres 16 plus Jaeger, all in Docker via Colima), a `SMOKE=1` run
(50 requests per second for 30 seconds) came back with 0 failed requests, 100
percent checks passing, and p99 around 180 milliseconds.

The numbers below are from after the multi-tenant spread (this script now
spreads load across 8 tenants instead of posting everything to one): a full,
non-smoke run (`make load` followed by `k6 run test/load/post_transactions.js`)
on that same machine completed both scenarios with 0 failed requests out of
38,417 and 100 percent checks passing: fanout held 500 requests per second
with p99 at 4.68 milliseconds, and hot_account held 200 requests per second
with p99 at 4.98 milliseconds. The app logs for that run show zero 503s, zero
500s, and zero serialization or retries-exhausted messages, with transactions
spread evenly across all 8 tenants (roughly 4,700 to 5,500 each). Before this
change, driving 500 requests per second at a single tenant ran into the
audit hash chain's in-process mutex, which serializes same-tenant transaction
posts and exhausted its serialization retries under that load; spreading
across tenants removes that bottleneck. Those numbers are far under the 100
and 150 millisecond thresholds, but they came from a single-node Compose
stack with everything, app, Postgres, and k6 itself, on one machine talking
over localhost, which is a best case. Your numbers on a different machine, or
with the components on separate hosts, will differ. Rerun it locally rather
than trusting these figures for your own setup.
