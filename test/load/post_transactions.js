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

import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const SMOKE = __ENV.SMOKE === '1';

// The load-test Compose stack provisions a high-limit key for the load
// tenant at startup from LOAD_TEST_API_KEY (see docker-compose.yml and
// ADR-012, "Per-key rate limiting"). The default here matches that compose
// value so `make load` works with no extra setup; API_KEY overrides it for
// CI or any other stack.
const API_KEY = __ENV.API_KEY || 'glk_loadtest_fixed_local_key';
const AUTH_HEADERS = { Authorization: `Bearer ${API_KEY}` };

// Pool of accounts used for the fan-out scenario. The hot_account scenario
// uses one additional, dedicated account created in setup().
const FANOUT_ACCOUNTS = 20;

// Fraction of requests that deliberately reuse a prior Idempotency-Key (with
// the exact same request body) instead of a fresh one, to exercise the
// replay path alongside the normal write path.
const DUPLICATE_KEY_PROB = 0.05;
const KEY_HISTORY_MAX = 20;

// Per-VU history of {key, bodyStr} pairs used so a "duplicate" request can
// resend the identical body under the identical key (same key + different
// body is a conflict, not a replay). k6 runs each VU as its own JS runtime,
// so this module-level array is private to a VU and persists across that
// VU's iterations, which is exactly the reuse window we want.
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
      //   fanout      ramps up to and sustains 500 requests/sec across many
      //               accounts for 1 minute.
      //   hot_account holds a steady rate against one fixed account (one
      //               leg pinned, the other leg fanned out) to exercise
      //               row-lock contention on that account's balance.
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

// setup() runs once before any VU traffic starts. It creates the real
// accounts the scenarios post transactions between, since the API has no
// fake-id shortcut: every posting's account_id must exist and match the
// transaction's currency.
export function setup() {
  const accounts = [];
  for (let i = 0; i < FANOUT_ACCOUNTS; i++) {
    const res = http.post(
      `${BASE}/v1/accounts`,
      JSON.stringify({ name: `k6-load-acct-${i}`, type: 'asset', currency: 'USD' }),
      { headers: { 'Content-Type': 'application/json', ...AUTH_HEADERS } }
    );
    if (res.status !== 201) {
      throw new Error(`setup: failed to create fanout account ${i}: ${res.status} ${res.body}`);
    }
    accounts.push(JSON.parse(res.body).id);
  }

  const hotRes = http.post(
    `${BASE}/v1/accounts`,
    JSON.stringify({ name: 'k6-load-hot-account', type: 'asset', currency: 'USD' }),
    { headers: { 'Content-Type': 'application/json', ...AUTH_HEADERS } }
  );
  if (hotRes.status !== 201) {
    throw new Error(`setup: failed to create hot account: ${hotRes.status} ${hotRes.body}`);
  }
  const hotAccount = JSON.parse(hotRes.body).id;

  return { accounts, hotAccount };
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
// gets -amount, both in USD. A small fraction of calls deliberately resend
// a prior key with its original body, to exercise the replay path.
function postTransaction(debitAcct, creditAcct, amount) {
  let key;
  let bodyStr;

  if (keyHistory.length > 0 && Math.random() < DUPLICATE_KEY_PROB) {
    const prior = keyHistory[Math.floor(Math.random() * keyHistory.length)];
    key = prior.key;
    bodyStr = prior.bodyStr;
  } else {
    key = uniqueKey();
    bodyStr = JSON.stringify({
      currency: 'USD',
      postings: [
        { account_id: debitAcct, amount: amount, description: 'k6 load debit' },
        { account_id: creditAcct, amount: -amount, description: 'k6 load credit' },
      ],
    });
    keyHistory.push({ key, bodyStr });
    if (keyHistory.length > KEY_HISTORY_MAX) {
      keyHistory.shift();
    }
  }

  const res = http.post(`${BASE}/v1/transactions`, bodyStr, {
    headers: { 'Content-Type': 'application/json', 'Idempotency-Key': key, ...AUTH_HEADERS },
  });

  // The server returns 201 both for a fresh write and for a replayed
  // request (distinguished by the Idempotent-Replayed response header), so
  // a single status check covers both paths.
  check(res, {
    'status is 201': (r) => r.status === 201,
  });
}

// fanout: pick two distinct accounts out of the pool at random for every
// request, spreading writes (and their row locks) across many accounts.
export function fanout(data) {
  const accounts = data.accounts;
  const i = Math.floor(Math.random() * accounts.length);
  let j = Math.floor(Math.random() * accounts.length);
  while (j === i) {
    j = Math.floor(Math.random() * accounts.length);
  }
  postTransaction(accounts[i], accounts[j], 100);
}

// hotAccount: one leg is always the same dedicated account, the other leg
// is fanned across the pool, so every request contends for the same
// account's balance and row lock.
export function hotAccount(data) {
  const accounts = data.accounts;
  const other = accounts[Math.floor(Math.random() * accounts.length)];
  postTransaction(data.hotAccount, other, 100);
}

// Default export, used if this script is ever run without an explicit
// scenario (e.g. `k6 run` with no SMOKE/scenario override).
export default function (data) {
  fanout(data);
}
