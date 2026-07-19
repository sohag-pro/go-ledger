# ADR-018: PII Crypto-Shredding (Reconciling Immutability with the Right to Erasure)

## Status

Accepted: 2026-07-11
Referenced by ADR-015 (audit remediation, Phase 6). Closes audit finding A9.3.

## Context

A ledger has two obligations that pull in opposite directions.

- **Immutability.** Postings are append-only, and every posted transaction extends
  a per-tenant tamper-evident hash chain (ADR-012, ADR-017). A row, once written,
  must never change, or the chain stops verifying. Money records are retained
  immutably for regulatory and audit reasons.
- **The right to erasure.** A data subject can ask for their personal data to be
  deleted. The free-text `description` on a posting is the field most likely to
  carry personal data ("rent to Jane Doe, 12 Elm St").

You cannot satisfy an erasure request by deleting or rewriting the row: that breaks
the append-only invariant and the hash chain. The description also lives a second
time inside the audit snapshot (`audit_log.after`), whose bytes are what the
`row_hash` is computed over, so scrubbing the plaintext there would invalidate the
chain.

## Decision

### 1. Crypto-shredding: erase the key, not the row

Encrypt the PII (posting descriptions) at rest with a key that can be destroyed.
"Erasing" a subject is destroying the key, which makes the ciphertext permanently
unreadable while every byte on disk stays exactly where it was. The row is never
mutated, so the append-only invariant and the hash chain are untouched.

- The description is encrypted **once** at post time (AES-256-GCM, a fresh random
  nonce), and the **same ciphertext string** is stored in both `postings.description`
  and the audit snapshot. The `row_hash` is therefore computed over ciphertext, and
  because verification recomputes the hash over the **stored bytes** and never
  decrypts, destroying the key leaves the chain fully verifiable. This is the
  load-bearing property: the money-critical test posts, chains, verifies (Valid),
  shreds the key, and verifies again (still Valid), with balances byte-identical.
- Money data (amounts, currency, account and transaction ids, timestamps) is
  **never** encrypted: it is immutable and required to derive balances and to
  verify the chain.
- The idempotency fingerprint is **unchanged**. It is a SHA-256 of the live
  request computed at post/retry time and stored only as an irreversible hash; it
  reveals no plaintext and is unaffected by a shred.

### 2. Envelope encryption with versioned per-tenant keys

- A master key (from the environment) wraps a per-tenant Data Encryption Key (DEK).
  The wrapped DEK lives in `crypto_keys`; the master key never touches the database.
- **DEKs are versioned.** A tenant can hold a sequence of DEK versions over time. A
  ciphertext is self-describing (`enc:v1:<version>:<base64(nonce||ct)>`), so a
  reader knows which DEK version to unwrap, and legacy plaintext (no `enc:` prefix,
  from before this feature) is passed through unchanged.
- **A shred destroys the current version's key and opens a new one.** Shredding
  stamps the current DEK version destroyed (its wrapped bytes removed) and lets the
  next write mint a fresh version. All data encrypted under the destroyed version
  is permanently unreadable (erased); the tenant keeps operating, with new writes
  protected by the new version. This is what makes a shred an *erasure of past PII*
  rather than a *bricking of the tenant*: system-generated narration (a conversion
  leg's label, a reversal's "reversal of X") is not personal data, and a tenant
  that erased one subject's history must still be able to post, convert, and
  reverse.
- Reading a description whose DEK version was shredded returns a redacted marker
  (`"[redacted: erased]"`) without error, so reads keep working after an erasure.
- Cross-tenant isolation: each tenant has its own DEK, and `crypto_keys` carries
  row-level security (ADR-015 Phase 5, reusing migration 0024's FORCE plus
  allow-when-unset pattern), so one tenant can never decrypt another's data.

### 3. Granularity: per-tenant in v1, per-party as the growth path

v1 shreds at the **tenant** grain (one DEK sequence per tenant). This demonstrates
and delivers the crypto-shred mechanism, but a real GDPR erasure targets one
customer, not a whole tenant. The growth path is a per-party (per-subject) DEK,
keyed off the party reference on accounts (ADR-015 Phase 6), so a single subject's
descriptions can be erased without touching anyone else's. The versioned-envelope
design above extends to that grain without changing the hash-chain reasoning.

### 4. Retention

Money and ledger data are retained immutably (regulatory). PII (descriptions) is
crypto-shreddable on an erasure request. The hash chain remains verifiable after
erasure (it hashes ciphertext). A shred is irreversible. The operation is admin-
scoped and requires an explicit confirmation.

## Consequences

- The immutability-vs-erasure tension is resolved without weakening either side:
  no row is ever mutated, the chain always verifies, and PII can still be erased.
- Operational prerequisites: a `LEDGER_MASTER_KEY` must be set in production; losing
  it makes all descriptions unreadable (it is a recovery dependency, stored with
  the other operator secrets). When the key is unset (dev/CI without it),
  descriptions are stored plaintext and the feature is inert, logged as a warning.
- Master-key rotation (re-wrapping DEKs under a new master key) is out of scope for
  v1; the wrapped-DEK indirection makes it a future operation that never touches
  ciphertext.
- A shred is coarse in v1 (whole tenant). The versioned design keeps the tenant
  operational after a shred, but per-subject erasure needs the per-party DEK grain
  noted above.
- `ErrTenantKeyShredded` should no longer reach the post path now that a shred opens
  a new version; it remains a mapped client error for any residual case rather than
  a generic 500.
