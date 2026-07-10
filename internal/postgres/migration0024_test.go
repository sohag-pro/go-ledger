package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// rlsProtectedTables is the exact table list migration 0024 puts under
// FORCE ROW LEVEL SECURITY (Task 5.4b, audit A3.5). tenants and
// webhook_fanout_cursor are deliberately excluded (see the migration's own
// doc comment): tenants is admin-managed and keyed by id, not a
// tenant_id-scoped child row, and webhook_fanout_cursor is a singleton with
// no tenant_id at all.
var rlsProtectedTables = []string{
	"accounts", "transactions", "postings", "idempotency_keys",
	"audit_log", "audit_outbox", "webhook_subscriptions", "webhook_deliveries",
	"fx_rates",
}

// TestMigration0024_RowLevelSecurity proves the RLS defense in depth added
// by migration 0024 actually protects the app request path, using its own
// dedicated container (not the package's shared one, see TestMain in
// repository_test.go) so it is free to reassign table ownership without
// disturbing every other test in this package.
//
// The migration-running "ledger" role (tcpostgres.WithUsername("ledger"))
// is Postgres's initdb bootstrap role: it is a superuser. Testing RLS
// through it, or through any other superuser, would prove nothing at all:
// superusers bypass row-level security unconditionally, policies or not
// (the brief's "superuser trap"). So this test creates a brand-new,
// non-superuser role and reassigns ownership of every RLS-protected table
// to it, mirroring how the goledger role owns these tables in production.
// That also exercises the "FORCE trap": without ALTER TABLE ... FORCE ROW
// LEVEL SECURITY, Postgres exempts a table's OWNER from its own policies,
// so a naive ENABLE-only migration would still let this owning,
// non-superuser role see every tenant's rows. Migration 0024 sets FORCE on
// every table, and this test's very first assertion (the forgotten-filter
// case) would fail without it.
func TestMigration0024_RowLevelSecurity(t *testing.T) {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping integration test: cannot start postgres container (is Docker running?): %v", err)
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// A brand-new, non-superuser role, made the OWNER of every
	// RLS-protected table (the FORCE trap: without FORCE, an owning role
	// bypasses RLS regardless of superuser status). It also needs plain
	// DML privileges on tenants (not RLS-protected, but referenced by FK
	// and read by CreateWebhookSubscription/InsertFXRate's tenant-exists
	// check) and on every sequence (fx_rates.id is bigserial), since
	// ownership transfer of a table does not carry its dependent sequence
	// with it.
	const roleName = "rls_app_role"
	const rolePassword = "rls-app-role-pw" //nolint:gosec // test-only password for a throwaway container role
	mustExecDB(t, sqlDB, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, roleName, rolePassword))
	for _, tbl := range rlsProtectedTables {
		mustExecDB(t, sqlDB, fmt.Sprintf(`ALTER TABLE %s OWNER TO %s`, tbl, roleName))
	}
	mustExecDB(t, sqlDB, fmt.Sprintf(`GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO %s`, roleName))
	mustExecDB(t, sqlDB, fmt.Sprintf(`GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO %s`, roleName))

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container mapped port: %v", err)
	}
	appDSN := fmt.Sprintf("postgres://%s:%s@%s:%s/ledger?sslmode=disable", roleName, rolePassword, host, mappedPort.Port())

	appPool, err := postgres.NewPool(ctx, appDSN, 5)
	if err != nil {
		t.Fatalf("new pool as %s: %v", roleName, err)
	}
	defer appPool.Close()

	// Confirm the role this whole test hinges on is genuinely not a
	// superuser: if this ever drifted true, every assertion below would be
	// hollow (see the doc comment above).
	var isSuperuser bool
	if err := appPool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&isSuperuser); err != nil {
		t.Fatalf("check rolsuper: %v", err)
	}
	if isSuperuser {
		t.Fatalf("test role %s must not be a superuser, or this test proves nothing", roleName)
	}

	repo := postgres.NewRepository(appPool)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenantA, "rls tenant A"); err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	if err := repo.CreateTenant(ctx, tenantB, "rls tenant B"); err != nil {
		t.Fatalf("create tenant B: %v", err)
	}

	txnA, debitA, _ := seedTxn(t, repo, tenantA)
	txnB, _, _ := seedTxn(t, repo, tenantB)

	// --- End-to-end through the repository: the GUC-set request path ---
	//
	// GetAccount and ListTransactions both now run inside withTenant
	// (repository.go), which sets app.tenant_id for the duration of a
	// dedicated transaction. Proving these still return tenant A's own
	// data through this brand-new, RLS-restricted, non-superuser
	// connection is the regression check: if any nested query inside that
	// wrapping (for example transactionFromRow's postings fetch) had been
	// left running on an unwrapped connection or a mismatched tenant, this
	// would come back empty instead.
	acct, err := repo.GetAccount(ctx, tenantA, debitA)
	if err != nil {
		t.Fatalf("GetAccount tenant A (GUC-set path): %v", err)
	}
	if acct.ID != debitA {
		t.Errorf("GetAccount tenant A: got id %q, want %q", acct.ID, debitA)
	}
	txns, err := repo.ListTransactions(ctx, tenantA, domain.TransactionFilter{}, nil, 50)
	if err != nil {
		t.Fatalf("ListTransactions tenant A (GUC-set path): %v", err)
	}
	if len(txns) != 1 || txns[0].Transaction.ID != txnA {
		t.Errorf("ListTransactions tenant A: got %d rows, want 1 matching %q", len(txns), txnA)
	}

	// --- The forgotten-filter case: RLS blocks the cross-tenant leak ---
	//
	// A raw "SELECT * FROM postings" with NO WHERE tenant_id at all,
	// issued directly against appPool (the same non-superuser, RLS-owner
	// role the repository itself uses) with app.tenant_id set to tenant A.
	// Without RLS this returns every tenant's postings; with FORCE RLS and
	// the tenant_isolation policy, it must return only tenant A's.
	rows := rawQueryAsTenant(ctx, t, appPool, tenantA, `SELECT tenant_id::text FROM postings`)
	assertAllTenant(t, "postings (forgotten filter)", rows, tenantA)
	if len(rows) == 0 {
		t.Error("postings (forgotten filter): expected tenant A's own postings, got none")
	}

	rowsAcct := rawQueryAsTenant(ctx, t, appPool, tenantA, `SELECT tenant_id::text FROM accounts`)
	assertAllTenant(t, "accounts (forgotten filter)", rowsAcct, tenantA)

	rowsTxn := rawQueryAsTenant(ctx, t, appPool, tenantA, `SELECT tenant_id::text FROM transactions`)
	assertAllTenant(t, "transactions (forgotten filter)", rowsTxn, tenantA)

	// --- WITH CHECK: a write into another tenant is rejected ---
	//
	// With the GUC set to tenant A, attempt to INSERT a new account row
	// whose tenant_id is tenant B. The tenant_isolation policy's WITH
	// CHECK clause must reject it: current_setting is set (to A), and the
	// row being inserted claims tenant_id = B, so neither the "unset"
	// branch nor the "matches" branch of the predicate is satisfied.
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO accounts (id, tenant_id, name, type, currency) VALUES ($1::uuid, $2::uuid, 'cross-tenant', 'asset', 'USD')`,
		uuid.NewString(), tenantB,
	); err == nil {
		t.Error("WITH CHECK: insert of a tenant B row while app.tenant_id=A was accepted, want a row-level security violation")
	}
	// An UPDATE that would move an existing tenant A row's tenant_id to B
	// is rejected the same way (WITH CHECK applies to UPDATE's new row
	// image too).
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`UPDATE accounts SET tenant_id = $1::uuid WHERE id = $2::uuid`,
		tenantB, debitA,
	); err == nil {
		t.Error("WITH CHECK: update moving tenant A's account to tenant B while app.tenant_id=A was accepted, want a row-level security violation")
	}
	// A correctly tenant-matched insert still succeeds (the policy is
	// restrictive, not simply broken).
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO accounts (id, tenant_id, name, type, currency) VALUES ($1::uuid, $2::uuid, 'same-tenant', 'asset', 'USD')`,
		uuid.NewString(), tenantA,
	); err != nil {
		t.Errorf("WITH CHECK: insert of a matched tenant A row while app.tenant_id=A was rejected: %v", err)
	}

	// --- GUC unset: the trusted background worker path keeps working ---
	//
	// The same non-superuser, RLS-owner role, but with app.tenant_id never
	// set on the connection: every policy's "current_setting(...) IS NULL"
	// branch is satisfied, so the read is unrestricted, exactly what the
	// audit chainer, webhook fan-out/delivery worker, idempotency sweep,
	// and restore-verify all depend on.
	rowsUnset := rawQueryNoTenant(ctx, t, appPool, `SELECT DISTINCT tenant_id::text FROM postings`)
	if !containsTenant(rowsUnset, tenantA) || !containsTenant(rowsUnset, tenantB) {
		t.Errorf("postings with GUC unset: got tenants %v, want both %q and %q (worker cross-tenant access must keep working)", rowsUnset, tenantA, tenantB)
	}

	// --- fx_rates: a tenant sees its own rows AND the global rows ---
	globalRate := int64(150_000_000) // 1.5 scaled by domain.RateScale (1e8)
	if err := repo.InsertFXRate(ctx, nil, "USD", "EUR", globalRate, 25, "test-global", nil); err != nil {
		t.Fatalf("insert global fx rate: %v", err)
	}
	tenantARate := int64(160_000_000)
	if err := repo.InsertFXRate(ctx, &tenantA, "USD", "EUR", tenantARate, 10, "test-tenant-a", nil); err != nil {
		t.Fatalf("insert tenant A fx rate: %v", err)
	}
	tenantBRate := int64(170_000_000)
	if err := repo.InsertFXRate(ctx, &tenantB, "USD", "EUR", tenantBRate, 10, "test-tenant-b", nil); err != nil {
		t.Fatalf("insert tenant B fx rate: %v", err)
	}
	fxCount := rawQueryAsTenant(ctx, t, appPool, tenantA, `SELECT mid_rate_e8::text FROM fx_rates WHERE base = 'USD' AND quote = 'EUR'`)
	wantGlobal, wantOwn, wantOther := fmt.Sprintf("%d", globalRate), fmt.Sprintf("%d", tenantARate), fmt.Sprintf("%d", tenantBRate)
	sawGlobal, sawOwn := false, false
	for _, v := range fxCount {
		switch v {
		case wantGlobal:
			sawGlobal = true
		case wantOwn:
			sawOwn = true
		case wantOther:
			t.Error("fx_rates: tenant A saw tenant B's tenant-specific rate")
		}
	}
	if !sawGlobal {
		t.Error("fx_rates: tenant A did not see the global (tenant_id NULL) rate")
	}
	if !sawOwn {
		t.Error("fx_rates: tenant A did not see its own tenant-specific rate")
	}
	if len(fxCount) != 2 { // tenant A's own row + the global row, not tenant B's
		t.Errorf("fx_rates: tenant A saw %d rows for USD/EUR, want 2 (own + global)", len(fxCount))
	}

	// --- The worker paths still process every tenant end to end ---
	//
	// The chainer and restore-verify's own audit walk never go through
	// withTenant/RunInTx's GUC-setting: they run their own queries
	// directly on the pool (audit.Chainer) or call
	// ledger.AuditService.Verify per tenant with the GUC left unset by
	// this repository's own withTenant helper never having touched their
	// connection. Both must still see every tenant's rows through this
	// same RLS-owner role.
	if err := repo.RunInTx(ctx, tenantA, func(ctx context.Context, tx domain.Tx) error {
		return tx.AppendAuditOutbox(ctx, tenantA, domain.AuditEvent{
			Action: domain.ActionTransactionCreated, TransactionID: txnA, Actor: "tester",
			After: []byte(`{"id":"` + txnA + `"}`),
		})
	}); err != nil {
		t.Fatalf("append audit outbox tenant A: %v", err)
	}
	if err := repo.RunInTx(ctx, tenantB, func(ctx context.Context, tx domain.Tx) error {
		return tx.AppendAuditOutbox(ctx, tenantB, domain.AuditEvent{
			Action: domain.ActionTransactionCreated, TransactionID: txnB, Actor: "tester",
			After: []byte(`{"id":"` + txnB + `"}`),
		})
	}); err != nil {
		t.Fatalf("append audit outbox tenant B: %v", err)
	}

	chainer := audit.NewChainer(appPool, discardTestLogger(), time.Millisecond, 500)
	if _, err := chainer.DrainOnce(ctx); err != nil {
		t.Fatalf("chainer drain (cross-tenant worker path): %v", err)
	}
	auditSvc := ledger.NewAuditService(repo)
	resultA, err := auditSvc.Verify(ctx, tenantA)
	if err != nil {
		t.Fatalf("verify tenant A: %v", err)
	}
	if !resultA.Valid || resultA.Checked == 0 {
		t.Errorf("verify tenant A: got valid=%v checked=%d, want a valid non-empty chain", resultA.Valid, resultA.Checked)
	}
	resultB, err := auditSvc.Verify(ctx, tenantB)
	if err != nil {
		t.Fatalf("verify tenant B: %v", err)
	}
	if !resultB.Valid || resultB.Checked == 0 {
		t.Errorf("verify tenant B: got valid=%v checked=%d, want a valid non-empty chain", resultB.Valid, resultB.Checked)
	}
	if _, err := repo.ListAuditByTransaction(ctx, tenantB, txnA); err != nil {
		t.Fatalf("list audit by transaction (tenant B, txn A, expect empty not error): %v", err)
	}
}

// rawQueryAsTenant opens its own transaction on pool, sets app.tenant_id to
// tenantID (transaction-local), runs query, and returns every row's single
// text column, committing before returning. It exists to exercise RLS
// exactly as a raw, filterless query would on the app's own connection role,
// independent of anything internal/postgres/repository.go does.
func rawQueryAsTenant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tenantID, query string) []string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	rows, err := tx.Query(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	return out
}

// rawQueryNoTenant is rawQueryAsTenant without ever calling set_config: the
// GUC stays unset on this transaction, the same state a background worker's
// own connection is in.
func rawQueryNoTenant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string) []string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	return out
}

// rawExecAsTenant opens its own transaction on pool, sets app.tenant_id to
// tenantID, runs the statement, and commits (or rolls back and returns the
// error, e.g. a row-level security violation from WITH CHECK).
func rawExecAsTenant(ctx context.Context, pool *pgxpool.Pool, tenantID, stmt string, args ...any) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("set_config: %w", err)
	}
	if _, err := tx.Exec(ctx, stmt, args...); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// assertAllTenant fails t if any row in got is not want, the shape of the
// forgotten-filter assertion repeated across accounts/transactions/postings.
func assertAllTenant(t *testing.T, label string, got []string, want string) {
	t.Helper()
	for _, tenantID := range got {
		if tenantID != want {
			t.Errorf("%s: saw tenant_id %q, want only %q (RLS should have blocked this cross-tenant row)", label, tenantID, want)
		}
	}
}

// containsTenant reports whether tenantID appears in list.
func containsTenant(list []string, tenantID string) bool {
	for _, v := range list {
		if v == tenantID {
			return true
		}
	}
	return false
}

// TestMigration0024_Reversible proves migration 0024 is cleanly reversible,
// the same up/down/up pattern the other single-migration tests in this
// package use (see migration0023_test.go): down must actually disable RLS
// and drop every tenant_isolation policy, not merely return without error,
// and up must re-apply cleanly afterward.
func TestMigration0024_Reversible(t *testing.T) {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping integration test: cannot start postgres container (is Docker running?): %v", err)
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	// Up through 0023: no RLS yet.
	if err := goose.UpTo(sqlDB, "migrations", 23); err != nil {
		t.Fatalf("migrate to 0023: %v", err)
	}
	assertPolicyCount(t, sqlDB, 0)
	assertForceCount(t, sqlDB, 0)

	// Up to 0024: every protected table gets ENABLE + FORCE + one policy.
	if err := goose.UpTo(sqlDB, "migrations", 24); err != nil {
		t.Fatalf("migrate to 0024: %v", err)
	}
	assertPolicyCount(t, sqlDB, len(rlsProtectedTables))
	assertForceCount(t, sqlDB, len(rlsProtectedTables))

	// Down to 0023: RLS must actually be gone, not just "no error".
	if err := goose.DownTo(sqlDB, "migrations", 23); err != nil {
		t.Fatalf("migrate down to 0023: %v", err)
	}
	assertPolicyCount(t, sqlDB, 0)
	assertForceCount(t, sqlDB, 0)

	// Up again: re-applies cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 24); err != nil {
		t.Fatalf("migrate up to 0024 again: %v", err)
	}
	assertPolicyCount(t, sqlDB, len(rlsProtectedTables))
	assertForceCount(t, sqlDB, len(rlsProtectedTables))
}

// assertPolicyCount fails t unless exactly want rows exist in pg_policies
// (one tenant_isolation policy per RLS-protected table when applied, zero
// once migration 0024 is rolled back).
func assertPolicyCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT count(*) FROM pg_policies WHERE policyname = 'tenant_isolation'`).Scan(&got); err != nil {
		t.Fatalf("count policies: %v", err)
	}
	if got != want {
		t.Errorf("pg_policies tenant_isolation count: got %d, want %d", got, want)
	}
}

// assertForceCount fails t unless exactly want tables have
// relforcerowsecurity set (pg_class), the FORCE half of migration 0024.
func assertForceCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_class WHERE relforcerowsecurity AND relname = ANY($1)`,
		toTextArray(rlsProtectedTables),
	).Scan(&got); err != nil {
		t.Fatalf("count forced tables: %v", err)
	}
	if got != want {
		t.Errorf("pg_class relforcerowsecurity count: got %d, want %d", got, want)
	}
}

// toTextArray renders a Go string slice as a Postgres text array literal
// (database/sql has no native []string binding for a plain "pgx" driver
// query like the one above).
func toTextArray(ss []string) string {
	out := "{"
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out + "}"
}
