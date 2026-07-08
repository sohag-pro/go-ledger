// k6 load test for go-ledger's POST /v1/transactions path.
//
// Run against the load-test Compose stack (see test/load/README.md):
//   make load
// or manually:
//   docker compose --profile load-test up -d --build
//   k6 run test/load/post_transactions.js
//
// SMOKE=1 shrinks this to a short, low-RPS profile suitable for CI:
//   SMOKE=1 k6 run test/load/post_transactions.js
//
// No remote k6 lib imports are used (the standard jslib.k6.io uuid helper is
// avoided) so this script has no network dependency at build time. Unique
// idempotency keys are generated inline from the VU id, iteration counter,
// a random suffix, and the clock, so they are unique across VUs and
// iterations without relying on Date.now() alone.
//
// Multi-tenant: each tenant's audit hash chain serializes same-tenant
// transaction posts through an in-process mutex (T13b), so a single
// tenant's throughput is bounded no matter how high its rate limit is. The
// load-test Compose stack provisions LOAD_TEST_TENANTS distinct tenants,
// each with its own high-limit key (see docker-compose.yml and main.go's
// provisionAPIKeys). This script spreads every request across those
// tenants, picking one at random per iteration, so aggregate throughput can
// scale with tenant count instead of being capped by one tenant's chain.

import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const SMOKE = __ENV.SMOKE === '1';

// The load-test Compose stack provisions a base high-limit key
// (LOAD_TEST_API_KEY) and derives one key per tenant from it as
// "<base>-t<i>" (see docker-compose.yml and ADR-012, "Per-key rate
// limiting"). The default here matches that compose value so `make load`
// works with no extra setup; API_KEY overrides it for CI or any other
// stack.
const BASE_KEY = __ENV.API_KEY || 'glk_loadtest_fixed_local_key';

// Number of tenants to spread load across. SMOKE keeps this small (just
// enough to prove the multi-tenant plumbing works) so the CI smoke run
// stays fast; the full profile defaults to matching docker-compose's
// LOAD_TEST_TENANTS (8), overridable with LOAD_TENANTS.
const LOAD_TENANTS = parseInt(__ENV.LOAD_TENANTS || '8', 10);
const TENANT_COUNT = SMOKE ? Math.min(2, LOAD_TENANTS) : LOAD_TENANTS;
const TENANT_KEYS = [];
for (let i = 0; i < TENANT_COUNT; i++) {
  TENANT_KEYS.push(`${BASE_KEY}-t${i}`);
}

// Accounts created per tenant for the fan-out scenario. The hot_account
// scenario uses one additional, dedicated account per tenant, also created
// in setup().
const FANOUT_ACCOUNTS_PER_TENANT = SMOKE ? 5 : 20;

// Fraction of requests that deliberately reuse a prior Idempotency-Key (with
// the exact same request body) instead of a fresh one, to exercise the
// replay path alongside the normal write path.
const DUPLICATE_KEY_PROB = 0.05;
const KEY_HISTORY_MAX = 20;

// Per-VU history of {key, bodyStr, tenantKey} triples used so a "duplicate"
// request can resend the identical body under the identical key and tenant
// (same key + different body, or the wrong tenant's auth, is a conflict,
// not a replay). k6 runs each VU as its own JS runtime, so this
// module-level array is private to a VU and persists across that VU's
// iterations, which is exactly the reuse window we want.
let keyHistory = [];

export const options = SMOKE
  ? {
      // CI smoke profile: short and low rate. Gate on zero failed requests
      // and a loose latency ceiling only, nothing throughput related.
      scenarios: {
        smoke: {
          executor: 'constant-arrival-rate',
          rate: 50,
          timeUnit: '1s',
          duration: '30s',
          preAllocatedVUs: 50,
          maxVUs: 100,
          exec: 'fanout',
        },
      },
      thresholds: {
        http_req_failed: ['rate==0'],
        http_req_duration: ['p(99)<500'],
      },
    }
  : {
      // Local load profile: two scenarios back to back.
      //   fanout      ramps up to and sustains 500 requests/sec spread
      //               across many tenants and, within each tenant, many
      //               accounts.
      //   hot_account holds a steady rate against one fixed account per
      //               tenant (one leg pinned, the other leg fanned out) to
      //               exercise row-lock contention on that account's
      //               balance, again spread across tenants.
      scenarios: {
        fanout: {
          executor: 'ramping-arrival-rate',
          startRate: 0,
          timeUnit: '1s',
          preAllocatedVUs: 300,
          maxVUs: 800,
          stages: [
            { duration: '15s', target: 500 },
            { duration: '45s', target: 500 },
          ],
          exec: 'fanout',
        },
        hot_account: {
          executor: 'constant-arrival-rate',
          rate: 200,
          timeUnit: '1s',
          duration: '1m',
          preAllocatedVUs: 150,
          maxVUs: 400,
          startTime: '1m',
          exec: 'hotAccount',
        },
      },
      thresholds: {
        // Reported for visibility, not gating: failures matter locally but
        // are not made to abort a machine-dependent run.
        http_req_failed: ['rate<0.01'],
        // The 500 RPS / p99 under 100ms figure is a local, machine
        // dependent measurement, not an SLO. See README for context.
        'http_req_duration{scenario:fanout}': ['p(99)<100'],
        'http_req_duration{scenario:hot_account}': ['p(99)<150'],
      },
    };

// setup() runs once before any VU traffic starts. For each tenant key it
// creates the real accounts that tenant's requests post transactions
// between, since the API has no fake-id shortcut: every posting's
// account_id must exist, belong to the caller's tenant, and match the
// transaction's currency. Returns { tenants: [{ key, accounts, hotAccount }, ...] }.
export function setup() {
  const tenants = [];
  for (let t = 0; t < TENANT_COUNT; t++) {
    const key = TENANT_KEYS[t];
    const headers = { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` };

    const accounts = [];
    for (let i = 0; i < FANOUT_ACCOUNTS_PER_TENANT; i++) {
      const res = http.post(
        `${BASE}/v1/accounts`,
        JSON.stringify({ name: `k6-load-acct-t${t}-${i}`, type: 'asset', currency: 'USD' }),
        { headers }
      );
      if (res.status !== 201) {
        throw new Error(
          `setup: failed to create fanout account tenant ${t} idx ${i}: ${res.status} ${res.body}`
        );
      }
      accounts.push(JSON.parse(res.body).id);
    }

    const hotRes = http.post(
      `${BASE}/v1/accounts`,
      JSON.stringify({ name: `k6-load-hot-account-t${t}`, type: 'asset', currency: 'USD' }),
      { headers }
    );
    if (hotRes.status !== 201) {
      throw new Error(`setup: failed to create hot account tenant ${t}: ${hotRes.status} ${hotRes.body}`);
    }
    const hotAccount = JSON.parse(hotRes.body).id;

    tenants.push({ key, accounts, hotAccount });
  }

  return { tenants };
}

// Builds an idempotency key that is unique per VU, per iteration, without
// depending on Date.now() alone (VU id and iteration counter make it unique
// even if the clock has coarse resolution; the random suffix and timestamp
// add extra separation across a fresh k6 process run).
function uniqueKey() {
  const rand = Math.floor(Math.random() * 1e9);
  return `k6-${__VU}-${__ITER}-${rand}-${Date.now()}`;
}

// Posts a balanced two-leg transaction: debitAcct gets +amount, creditAcct
// gets -amount, both in USD, authenticated as tenantKey. A small fraction
// of calls deliberately resend a prior key with its original body and
// tenant, to exercise the replay path.
function postTransaction(tenantKey, debitAcct, creditAcct, amount) {
  let key;
  let bodyStr;
  let authKey = tenantKey;

  if (keyHistory.length > 0 && Math.random() < DUPLICATE_KEY_PROB) {
    const prior = keyHistory[Math.floor(Math.random() * keyHistory.length)];
    key = prior.key;
    bodyStr = prior.bodyStr;
    authKey = prior.tenantKey;
  } else {
    key = uniqueKey();
    bodyStr = JSON.stringify({
      currency: 'USD',
      postings: [
        { account_id: debitAcct, amount: amount, description: 'k6 load debit' },
        { account_id: creditAcct, amount: -amount, description: 'k6 load credit' },
      ],
    });
    keyHistory.push({ key, bodyStr, tenantKey });
    if (keyHistory.length > KEY_HISTORY_MAX) {
      keyHistory.shift();
    }
  }

  const res = http.post(`${BASE}/v1/transactions`, bodyStr, {
    headers: { 'Content-Type': 'application/json', 'Idempotency-Key': key, Authorization: `Bearer ${authKey}` },
  });

  // The server returns 201 both for a fresh write and for a replayed
  // request (distinguished by the Idempotent-Replayed response header), so
  // a single status check covers both paths.
  check(res, {
    'status is 201': (r) => r.status === 201,
  });
}

// fanout: pick a random tenant, then pick two distinct accounts out of that
// tenant's pool at random for every request. Spreading across tenants
// first, then across accounts within a tenant, is what lets aggregate
// throughput scale past any one tenant's serialized audit chain.
export function fanout(data) {
  const tenants = data.tenants;
  const tenant = tenants[Math.floor(Math.random() * tenants.length)];
  const accounts = tenant.accounts;
  const i = Math.floor(Math.random() * accounts.length);
  let j = Math.floor(Math.random() * accounts.length);
  while (j === i) {
    j = Math.floor(Math.random() * accounts.length);
  }
  postTransaction(tenant.key, accounts[i], accounts[j], 100);
}

// hotAccount: pick a random tenant, then always use that tenant's dedicated
// hot account for one leg, fanned across the tenant's pool for the other,
// so every request contends for that tenant's hot account balance and row
// lock, while the contention itself is still spread across tenants.
export function hotAccount(data) {
  const tenants = data.tenants;
  const tenant = tenants[Math.floor(Math.random() * tenants.length)];
  const other = tenant.accounts[Math.floor(Math.random() * tenant.accounts.length)];
  postTransaction(tenant.key, tenant.hotAccount, other, 100);
}

// Default export, used if this script is ever run without an explicit
// scenario (e.g. `k6 run` with no SMOKE/scenario override).
export default function (data) {
  fanout(data);
}
