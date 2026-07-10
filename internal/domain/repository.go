package domain

import (
	"context"
	"encoding/json"
	"time"
)

// StatementEntry is one line of an account statement: a posting that affected the
// account, with the account's running balance as of that posting. Amount and
// RunningBalance are in the account's currency.
type StatementEntry struct {
	// ID is the posting's own id. It is the keyset tiebreaker for paging and what
	// a statement cursor points at.
	ID             string
	TransactionID  string
	Amount         Money
	RunningBalance Money
	Description    string
	CreatedAt      time.Time
}

// StatementCursor is an opaque keyset position for paging a statement. It points
// at the last entry of the previous page; the next page returns entries strictly
// older than it, by (CreatedAt, ID) descending.
type StatementCursor struct {
	CreatedAt time.Time
	ID        string
}

// TransactionFilter narrows ListTransactions to a date range and/or an exact
// reference match (Task 4.4, audit A7.2). A nil field means "no filter on
// that dimension". From is inclusive (created_at >= From); To is exclusive
// (created_at < To), the half-open window a caller expects from a from/to
// range, so a transaction landing exactly on the To boundary is never
// double-counted across two adjacent calls that tile the same range.
type TransactionFilter struct {
	From      *time.Time
	To        *time.Time
	Reference *string
}

// TransactionListItem is one row of a keyset-paged transaction list (Task
// 4.4, audit A7.2): the transaction itself plus CreatedAt, the row's actual
// insert time. CreatedAt is not a Transaction field (EffectiveAt is a
// different, caller-supplied concept: the value date, which may not even be
// set), but it is exactly what the keyset cursor pages by, the same
// (CreatedAt, ID) shape StatementEntry already pages a statement by.
type TransactionListItem struct {
	Transaction Transaction
	CreatedAt   time.Time
}

// Tx is a unit of work bound to a single database transaction. The service
// layer composes one or more writes inside RunInTx; everything done through a Tx
// commits together or not at all.
type Tx interface {
	// CreateTransaction validates t and persists the transaction and all its
	// postings within the surrounding transaction. If t.ID is empty an id is
	// assigned and written back to t.
	CreateTransaction(ctx context.Context, tenantID string, t *Transaction) error

	// InsertIdempotencyKey records key with the request fingerprint, the
	// scheme that fingerprint was computed under (see
	// CurrentFingerprintScheme), and the transaction it produced, within the
	// surrounding transaction. expires_at is stamped as the DATABASE SERVER's
	// now() + ttl, never this process's clock (Task 4.5, audit A1.4,
	// consistent with the fx effective_at server-side stamping), so ttl
	// bounds how long the key blocks reuse before GetIdempotencyKey starts
	// treating it as absent.
	//
	// It returns ErrDuplicateIdempotencyKey if (tenantID, key) already
	// exists AND has not yet expired. If a row for (tenantID, key) exists
	// but HAS expired, this call transparently replaces it (an upsert, not
	// a plain insert) with the new fingerprint, scheme, transaction, and
	// expiry: an expired key is absent from the caller's point of view, so
	// the row it left behind must not cause a spurious duplicate on the
	// very next post that reuses the same key.
	InsertIdempotencyKey(ctx context.Context, tenantID, key, fingerprint, scheme, transactionID string, ttl time.Duration) error

	// AppendAuditOutbox writes one append-only audit_outbox row within the
	// surrounding transaction (ADR-017): the event is durable if and only if
	// the surrounding transaction commits. Unlike the old AppendAudit, this
	// never reads the tenant's chain head and never computes a hash: it is a
	// plain, conflict-free insert, which is what lets it run outside any
	// per-tenant serialization and stay correct across any number of
	// instances. The single background chainer (internal/audit.Chainer) is
	// what later reads this row and extends the tenant's tamper-evident hash
	// chain (see ComputeAuditRowHash and the chainer's doc comment).
	AppendAuditOutbox(ctx context.Context, tenantID string, e AuditEvent) error

	// TenantDailyDebits returns the tenant's already-posted per-currency
	// debit total for "today" (created_at >= date_trunc('day', now()) on the
	// DATABASE SERVER's clock), within the surrounding transaction (Task
	// 2.4b, audit A3.4). It is the daily-volume policy's read: called from
	// inside RunInTx's SERIALIZABLE transaction, so two concurrent posts for
	// the same tenant that would both cross the cap are a genuine read-write
	// antidependency SERIALIZABLE can detect and abort one of. Before
	// ADR-017 removed it, an in-process per-tenant mutex also serialized
	// these calls; that mutex existed for the audit hash chain's read, not
	// for this one, and its removal (RunInTx no longer holds any per-tenant
	// lock) leaves this read backed by SERIALIZABLE alone, the same backstop
	// RunInTx's own doc comment describes. The returned map is keyed by
	// currency code; a currency with no posted debits today is simply
	// absent, never a zero-valued entry, so a caller should treat a missing
	// key as 0 (see CheckTransactionPolicy).
	TenantDailyDebits(ctx context.Context, tenantID string) (map[string]int64, error)
}

// Repository is the persistence port for the ledger. The domain owns this
// contract; storage adapters (see internal/postgres) implement it. Every method
// is scoped by tenantID: the ledger is multi-tenant, and a tenant can only ever
// see its own accounts, transactions, and balances.
//
// Writes follow the core invariant: postings are append-only and balances are
// derived, never stored. CreateTransaction validates the double-entry invariant
// before persisting and writes the transaction and all its postings atomically.
type Repository interface {
	// CreateAccount persists a. If a.ID is empty the adapter assigns one and
	// writes it back to a. It returns the account's validation error if a is
	// invalid.
	CreateAccount(ctx context.Context, tenantID string, a *Account) error

	// GetAccount returns the account with the given id within the tenant, or
	// ErrAccountNotFound if none exists.
	GetAccount(ctx context.Context, tenantID, id string) (Account, error)

	// ListAccounts returns up to limit of the tenant's accounts, ordered by name.
	// Accounts are a small bounded set, so this is a simple capped list rather
	// than a paginated cursor.
	ListAccounts(ctx context.Context, tenantID string, limit int) ([]Account, error)

	// CreateTransaction validates t (the double-entry invariant) and persists the
	// transaction together with all its postings in a single atomic database
	// transaction. If t.ID is empty the adapter assigns one and writes it back to
	// t; the same applies to each posting's identity. It returns t's validation
	// error if t is invalid.
	CreateTransaction(ctx context.Context, tenantID string, t *Transaction) error

	// GetTransaction returns the transaction with the given id within the tenant,
	// including all its postings, or ErrTransactionNotFound if none exists.
	GetTransaction(ctx context.Context, tenantID, id string) (Transaction, error)

	// GetReversalOf returns the transaction that reverses originalID within
	// tenantID (its ReversesTransactionID equals originalID), including all
	// its postings, or ErrTransactionNotFound if no reversal exists yet
	// (Task 4.2, audit A1.2). The transactions_one_reversal_idx unique index
	// (migration 0017) guarantees there is at most one, so this is a plain
	// lookup, never a list: TransactionService.ReverseTransaction calls it
	// both as its idempotency precheck (before ever attempting to post a
	// reversal) and as the race guard's read-back after a concurrent
	// double-reverse loses the insert.
	GetReversalOf(ctx context.Context, tenantID, originalID string) (Transaction, error)

	// ListTransactions returns up to limit of the tenant's transactions
	// matching filter, newest first, keyset paged the same way Statement
	// pages postings (Task 4.4, audit A7.2): after is the keyset position to
	// page from, nil starts at the newest transaction. filter's From, To, and
	// Reference are each optional; nil disables that dimension. Every
	// returned transaction includes its own postings, fetched for the whole
	// page in one extra round trip rather than one query per transaction.
	ListTransactions(ctx context.Context, tenantID string, filter TransactionFilter, after *StatementCursor, limit int) ([]TransactionListItem, error)

	// GetOrCreateClearingAccount returns the tenant's per-currency FX clearing
	// system account (ADR-014), creating it on first use. name is reserved and
	// deterministic ("fx.clearing.<CURRENCY>"), so two callers converting the
	// same tenant's currency for the first time, even concurrently, resolve to
	// the same row rather than creating duplicates. The account is a Liability
	// type and is marked System (see Account.System): it is expected to carry a
	// permanent, often nonzero, open position, unlike a user account.
	GetOrCreateClearingAccount(ctx context.Context, tenantID string, currency Currency) (Account, error)

	// GetIdempotencyKey returns the stored record for (tenantID, key),
	// including the fingerprint scheme it was stored under (IdempotencyRecord.
	// Scheme), or ErrIdempotencyKeyNotFound if none exists. A row whose
	// expires_at has passed is treated exactly like no row at all (Task 4.5,
	// audit A1.4): it returns ErrIdempotencyKeyNotFound, whether or not the
	// background sweep (SweepExpiredIdempotencyKeys) has physically deleted
	// it yet, so an expired key behaves like a brand-new one to a caller.
	GetIdempotencyKey(ctx context.Context, tenantID, key string) (IdempotencyRecord, error)

	// ListAuditByTransaction returns the audit rows for a transaction, oldest
	// first. An unknown transaction yields no rows.
	ListAuditByTransaction(ctx context.Context, tenantID, transactionID string) ([]AuditEntry, error)

	// ListAuditByAccount returns up to limit audit rows for every transaction
	// that has a posting touching the account, newest first (keyset paged). after
	// is the keyset position to page from; nil starts at the newest entry. An
	// unknown account yields no rows.
	ListAuditByAccount(ctx context.Context, tenantID, accountID string, after *StatementCursor, limit int) ([]AuditEntry, error)

	// ListAuditForVerify returns every audit row for the tenant, oldest first
	// (created_at, id ascending), including PrevHash and RowHash. It is the
	// full per-tenant walk used to recompute and check the tamper-evident hash
	// chain end to end, not a paged read for display.
	ListAuditForVerify(ctx context.Context, tenantID string) ([]AuditEntry, error)

	// Balance returns the derived balance of an account: the sum of its postings'
	// signed amounts. It returns ErrAccountNotFound if the account does not exist.
	//
	// Balance is a non-snapshot read: the existence check and the sum are not
	// guaranteed to observe the same instant, so a balance read concurrent with a
	// posting may reflect either side of that write. This is fine for an
	// eventually-summed balance; a caller that needs a point-in-time consistent
	// read should perform it inside RunInTx.
	Balance(ctx context.Context, tenantID, accountID string) (Money, error)

	// Statement returns up to limit postings affecting the account, newest first,
	// each carrying the account's running balance as of that posting. after is the
	// keyset position to page from; nil starts at the newest entry. currency is the
	// account's currency, used to build the Money values. The caller is expected to
	// have resolved the account (for its currency and existence) first; an unknown
	// account simply yields no entries.
	Statement(ctx context.Context, tenantID, accountID string, currency Currency, after *StatementCursor, limit int) ([]StatementEntry, error)

	// RunInTx executes fn inside a SERIALIZABLE database transaction, scoped to
	// tenantID. It commits if fn returns nil and rolls back otherwise. Because
	// SERIALIZABLE can abort a transaction with a serialization conflict under
	// concurrency, the adapter retries fn automatically a bounded number of
	// times; fn must therefore be safe to run more than once. It returns the
	// last error if retries are exhausted, or any non-retryable error from fn.
	//
	// Until ADR-017, tenantID also picked an in-process per-tenant mutex the
	// adapter held for the whole call, because every post read the tenant's
	// audit chain head and extended it in the same transaction: two
	// concurrent same-tenant attempts racing on that read could repeatedly
	// abort each other with a serialization failure (SQLSTATE 40001) and
	// exhaust the retry budget, and worse, across more than one app instance
	// the mutex could not prevent a forked chain at all (ADR-017's Context).
	// ADR-017 removes the audit chain from the posting transaction entirely
	// (a post now writes an outbox row; a single background chainer builds
	// the chain asynchronously), which removes the reason for the mutex: it
	// is gone, and same-tenant calls now run fully concurrently, serialized
	// only by whatever SERIALIZABLE itself detects (the balance invariant and
	// the idempotency primary key, both still enforced in-transaction).
	RunInTx(ctx context.Context, tenantID string, fn func(context.Context, Tx) error) error

	// CountPendingOutbox returns the number of audit_outbox rows for tenantID
	// that the chainer has not yet processed (ADR-017): the audit chain's lag,
	// reported alongside the chained head by audit verify so a caller can see
	// whether the chain is current or behind. Zero means every event this
	// tenant has posted is already reflected in audit_log.
	CountPendingOutbox(ctx context.Context, tenantID string) (int, error)

	// SweepExpiredIdempotencyKeys deletes every idempotency_keys row whose
	// expires_at has passed, across every tenant, and returns how many rows
	// it deleted in total (Task 4.5, audit A1.4). It is not scoped to one
	// tenant and is never called from inside RunInTx: it is a plain
	// maintenance statement a background goroutine calls on an interval (see
	// cmd/server's idempotency sweeper), independent of any request's unit
	// of work. GetIdempotencyKey already treats an expired row as absent, so
	// this sweep is purely about reclaiming space, not correctness: a
	// deployment that never ran it would behave identically from a caller's
	// point of view, just with an ever-growing table. The implementation
	// deletes in bounded batches rather than one unbounded statement, so a
	// large backlog cannot lock and remove an arbitrarily large number of
	// rows in a single delete that contends with live posts.
	SweepExpiredIdempotencyKeys(ctx context.Context) (int64, error)

	// GetAPIKeyByHash resolves an unrevoked api_keys row by the SHA-256 hex hash
	// of a presented key, or ErrAPIKeyNotFound if no such unrevoked key exists.
	GetAPIKeyByHash(ctx context.Context, hash string) (APIKey, error)

	// InsertAPIKey persists k with keyHash as its stored credential. Only the
	// hash is ever written; the plaintext is never stored. k.Scopes and
	// k.ExpiresAt (Task 2.2b) are persisted as given; an empty k.Scopes
	// defaults to {read, post}, matching the api_keys.scopes column default,
	// so every caller that predates scopes keeps working unchanged.
	InsertAPIKey(ctx context.Context, k APIKey, keyHash string) error

	// GetAPIKeyByID returns the api_keys row with the given id, revoked or
	// not, or ErrAPIKeyNotFound if no such row exists (Task 2.2b). Unlike
	// GetAPIKeyByHash it does not filter on revoked_at or join tenants: it is
	// the admin surface's raw lookup, used to copy an existing key's
	// tenant/name/scopes when rotating it.
	GetAPIKeyByID(ctx context.Context, id string) (APIKey, error)

	// ListAPIKeysByTenant returns every api_keys row for tenantID, oldest
	// first, revoked or not (Task 2.2b): the admin surface's list view shows
	// a tenant's full key history. Never carries the plaintext (it is never
	// stored) or the hash.
	ListAPIKeysByTenant(ctx context.Context, tenantID string) ([]APIKey, error)

	// RevokeAPIKey sets revoked_at (if not already set) for the key
	// identified by id (Task 2.2b). It returns ErrAPIKeyNotFound if no key
	// matches id. Revoking an already-revoked key is a no-op success, not an
	// error: the caller's intent (this key must not work) is already true.
	RevokeAPIKey(ctx context.Context, id string) error

	// TouchAPIKeyLastUsed sets the last_used_at timestamp for the key
	// identified by id (Task 2.2). Called best-effort and throttled from the
	// auth resolver: not on every request, and its error is never allowed to
	// fail the request that triggered it.
	TouchAPIKeyLastUsed(ctx context.Context, id string, when time.Time) error

	// CreateTenant inserts a new tenant row, active by default. It returns
	// ErrTenantAlreadyExists if tenantID already has a row.
	CreateTenant(ctx context.Context, tenantID, name string) error

	// GetTenant returns the tenant with the given id, or ErrTenantNotFound if
	// none exists.
	GetTenant(ctx context.Context, tenantID string) (Tenant, error)

	// ListTenants returns up to limit tenants, oldest first. It is an
	// operator-facing listing, not scoped to any one tenant.
	ListTenants(ctx context.Context, limit int) ([]Tenant, error)

	// SetTenantStatus updates the tenant's status (the operator action that
	// suspends, closes, or reactivates a tenant). It returns ErrInvalidTenant
	// if status is not one of TenantStatus.Valid()'s three values, or
	// ErrTenantNotFound if no tenant matches tenantID.
	SetTenantStatus(ctx context.Context, tenantID string, status TenantStatus) error

	// InsertFXRate appends a new fx_rates row (Task 2.4, audit A3.3): fx_rates
	// stays append-only, so this is always a plain INSERT, never an update to
	// an existing row. tenantID nil makes the row the global default rate for
	// the pair; tenantID naming a tenant makes it that tenant's own rate,
	// resolved ahead of the global default (see fx.Provider.Rate and the
	// CurrentFXRate query, migration 0014). base and quote must be distinct,
	// valid three-letter currency codes; midRateE8 must be positive
	// (ErrNonPositiveRate otherwise) and spreadBps must be in [0, 10000)
	// (ErrInvalidSpread otherwise). It returns ErrTenantNotFound if tenantID
	// names a tenant that does not exist.
	//
	// effectiveAt is nil for the common "effective immediately" case: the
	// adapter must let the DATABASE SERVER's clock stamp the row (SQL
	// COALESCE(..., now()), never the caller's own time.Now()), because
	// CurrentFXRate's "effective_at <= now()" gate also runs on the server's
	// clock. Stamping with the caller's clock instead is a real clock-skew bug
	// (Task 2.4 remediation): a caller even slightly ahead of the database
	// server makes a just-inserted "immediate" rate transiently invisible,
	// silently falling through to the global default. A non-nil effectiveAt
	// (an explicit, possibly future, scheduled rate) is still honored exactly
	// as given.
	InsertFXRate(ctx context.Context, tenantID *string, base, quote Currency, midRateE8 int64, spreadBps int32, source string, effectiveAt *time.Time) error

	// SetTenantSettings overwrites the tenants.settings jsonb column for
	// tenantID with the given raw JSON document (Task 2.4b, audit A3.4). It
	// is a whole-document replace, not a merge: the only writer today
	// (admin.Service.SetTenantPolicy) always builds the full TenantSettings
	// document from the policy given, so there is nothing else in the
	// column yet worth preserving. It returns ErrTenantNotFound if tenantID
	// has no row.
	SetTenantSettings(ctx context.Context, tenantID string, settings json.RawMessage) error

	// CreateWebhookSubscription persists sub with secret as its stored HMAC
	// signing key (Task 4.1, audit A7.1). If sub.ID is empty the adapter
	// assigns one and writes it back to sub. Unlike InsertAPIKey/keyHash,
	// secret is stored as-is, never hashed: the delivery worker must read it
	// back in full to sign every outbound payload. It returns
	// domain.ErrTenantNotFound if sub.TenantID names a tenant that does not
	// exist (the webhook_subscriptions_tenant_id_fkey foreign key).
	CreateWebhookSubscription(ctx context.Context, sub *WebhookSubscription, secret string) error

	// ListWebhookSubscriptionsByTenant returns every webhook_subscriptions
	// row for tenantID, oldest first, active or not (Task 4.1): the admin
	// surface's list view. Never selects the secret column: it is shown once,
	// at creation time, and is never recoverable through a list call, the
	// same discipline ListAPIKeysByTenant follows for a key's plaintext.
	ListWebhookSubscriptionsByTenant(ctx context.Context, tenantID string) ([]WebhookSubscription, error)

	// SetWebhookSubscriptionActive sets active for the subscription
	// identified by id (Task 4.1). The admin surface's DeleteSubscription
	// calls this with active=false rather than issuing a hard DELETE: a
	// webhook_deliveries row references its subscription
	// (webhook_deliveries_subscription_id_fkey) with no ON DELETE cascade, so
	// a subscription with delivery history (which is the common case: the
	// whole point of a subscription is to accumulate deliveries) cannot be
	// hard-deleted without either losing that history or cascading the
	// delete into it. Deactivating achieves the caller-visible contract
	// ("delete stops future deliveries": the fan-out step only creates new
	// pending deliveries for active subscriptions, and the delivery worker
	// only attempts delivery for an active subscription's rows) while
	// keeping every already-created delivery row, and its audit trail,
	// intact. It returns domain.ErrWebhookSubscriptionNotFound if no
	// subscription matches id.
	SetWebhookSubscriptionActive(ctx context.Context, id string, active bool) error
}
