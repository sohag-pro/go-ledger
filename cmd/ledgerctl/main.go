// Command ledgerctl is the operator CLI over the admin surface (Task 2.2b,
// audit A3.2/A2.3): onboarding a tenant and issuing/rotating/revoking its api
// keys with no raw SQL. It is the bootstrap path for the REST admin API
// itself: since every /v1/admin/ route requires an admin-scoped key (see
// internal/auth/scope.go), an operator runs
//
//	ledgerctl key issue --tenant <id> --name bootstrap-admin --scopes admin
//
// once, against DATABASE_URL directly, to mint the first admin key, then
// uses the REST API (or ledgerctl itself) for everything after.
//
// A newly issued or rotated key's plaintext is printed to stdout exactly
// once, deliberately: it is never logged (this package never touches
// log/slog, unlike the rest of this codebase) and never stored anywhere,
// only its hash is.
//
// "rate set" (Task 2.4, audit A3.3) is the operator path for a tenant's own
// FX rate and spread: it appends a tenant-scoped fx_rates row, resolved
// ahead of the global default (see internal/fx.Provider.Rate). There is no
// REST equivalent yet.
//
// "tenant policy" (Task 2.4b, audit A3.4) sets a tenant's optional posting
// guardrails (a max transaction amount, a daily volume cap, a currency
// allowlist, each enforced per currency), the same policy the REST
// POST /v1/admin/tenants/{id}/policy endpoint sets. Every flag is optional
// and this command always writes the FULL policy the flags describe: an
// omitted flag clears (unlimits) that guardrail rather than leaving a
// previous value in place, so reapplying an existing limit means passing it
// again explicitly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, connectService); err != nil {
		fmt.Fprintln(os.Stderr, "ledgerctl: "+err.Error())
		os.Exit(1)
	}
}

// serviceFactory builds the admin.Service run talks to. It is a parameter
// (rather than run calling postgres.NewRepository directly) so tests can
// exercise argument parsing and subcommand dispatch without a real
// DATABASE_URL or database connection: see the fake in main_test.go.
type serviceFactory func(ctx context.Context) (*admin.Service, func(), error)

// connectService is the real serviceFactory: it reads DATABASE_URL, opens a
// pgx pool, and returns an admin.Service over the real postgres repository.
// The returned cleanup func closes the pool; the caller is expected to defer
// it.
func connectService(ctx context.Context) (*admin.Service, func(), error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, nil, errors.New("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	return admin.NewService(postgres.NewRepository(pool)), pool.Close, nil
}

// handler is one subcommand's implementation: parse args, call svc, print
// to out. It never touches svc until after args are known to be well-formed
// (see the parse* helpers below), so a bad flag fails before any database
// round trip.
type handler func(ctx context.Context, svc *admin.Service, out *os.File, args []string) error

// commands maps resource -> action -> handler. Looking a command up here
// happens before connectService is ever called (see run), so an unknown
// resource or action, or a request for `-h`, fails immediately with no
// DATABASE_URL or database dependency at all.
var commands = map[string]map[string]handler{
	"tenant": {
		"create": tenantCreate,
		"list":   tenantList,
		"status": tenantStatus,
		"policy": tenantPolicySet,
	},
	"key": {
		"issue":  keyIssue,
		"rotate": keyRotate,
		"revoke": keyRevoke,
		"list":   keyList,
	},
	"rate": {
		"set": rateSet,
	},
}

// usage is printed on a missing or unrecognized subcommand.
const usage = `usage: ledgerctl <resource> <action> [flags]

  tenant create --name NAME
  tenant list
  tenant status --id ID --status active|suspended|closed
  tenant policy --tenant ID [--max-amount N] [--daily-limit N] [--currencies USD,EUR]

  key issue --tenant ID --name NAME --scopes read,post[,admin] [--expires-in DURATION]
  key rotate --id KEYID
  key revoke --id KEYID
  key list --tenant ID

  rate set --tenant ID --base BASE --quote QUOTE --mid RATE [--spread-bps BPS] [--source SOURCE] [--effective-at RFC3339]

DATABASE_URL must be set.`

// resolveHandler looks up the handler for resource/action, or returns a
// usage error naming what was requested. Split out from run so subcommand
// dispatch is testable without touching serviceFactory at all.
func resolveHandler(resource, action string) (handler, error) {
	actions, ok := commands[resource]
	if !ok {
		return nil, fmt.Errorf("unknown resource %q\n\n%s", resource, usage)
	}
	h, ok := actions[action]
	if !ok {
		return nil, fmt.Errorf("unknown action %q for resource %q\n\n%s", action, resource, usage)
	}
	return h, nil
}

func run(args []string, out *os.File, newService serviceFactory) error {
	if len(args) < 2 {
		//nolint:staticcheck // usage is a multi-line help block, not a single sentence; ST1005's "no trailing punctuation" rule does not fit it
		return errors.New(usage)
	}
	h, err := resolveHandler(args[0], args[1])
	if err != nil {
		return err
	}

	ctx := context.Background()
	svc, cleanup, err := newService(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	return h(ctx, svc, out, args[2:])
}

// --- tenant subcommands ---

func tenantCreate(ctx context.Context, svc *admin.Service, out *os.File, args []string) error {
	fs := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	name := fs.String("name", "", "tenant name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}

	t, err := svc.CreateTenant(ctx, *name)
	if err != nil {
		return fmt.Errorf("create tenant: %w", err)
	}
	_, _ = fmt.Fprintf(out, "created tenant %s (%s) status=%s\n", t.ID, t.Name, t.Status)
	return nil
}

func tenantList(ctx context.Context, svc *admin.Service, out *os.File, args []string) error {
	fs := flag.NewFlagSet("tenant list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	tenants, err := svc.ListTenants(ctx)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	if len(tenants) == 0 {
		_, _ = fmt.Fprintln(out, "no tenants")
		return nil
	}
	for _, t := range tenants {
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\n", t.ID, t.Status, t.Name)
	}
	return nil
}

// parseTenantStatus parses --id and --status from a "tenant status"
// invocation and validates status against domain.TenantStatus.Valid(),
// before any database call. Exported to this file's tests as a pure
// function: no admin.Service or DATABASE_URL involved.
func parseTenantStatus(args []string) (id string, status domain.TenantStatus, err error) {
	fs := flag.NewFlagSet("tenant status", flag.ContinueOnError)
	idFlag := fs.String("id", "", "tenant id (required)")
	statusFlag := fs.String("status", "", "active|suspended|closed (required)")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if *idFlag == "" {
		return "", "", errors.New("--id is required")
	}
	st := domain.TenantStatus(*statusFlag)
	if !st.Valid() {
		return "", "", fmt.Errorf("--status must be one of active, suspended, closed (got %q)", *statusFlag)
	}
	return *idFlag, st, nil
}

func tenantStatus(ctx context.Context, svc *admin.Service, out *os.File, args []string) error {
	id, status, err := parseTenantStatus(args)
	if err != nil {
		return err
	}
	if err := svc.SetTenantStatus(ctx, id, status); err != nil {
		return fmt.Errorf("set tenant status: %w", err)
	}
	_, _ = fmt.Fprintf(out, "tenant %s is now %s\n", id, status)
	return nil
}

// parseTenantPolicy parses "tenant policy"'s flags into a domain.TenantPolicy
// (Task 2.4b, audit A3.4). Every flag is optional: an omitted flag is its
// zero value (0 for the two amount flags, empty for --currencies), which
// domain.TenantPolicy already treats as "no limit" for that guardrail. This
// command always sets the FULL policy from the flags given on this
// invocation, never merging with whatever was there before: an omitted flag
// CLEARS that guardrail rather than leaving a previous value in place. An
// operator who wants to keep an existing limit must pass it again explicitly
// (a plain SetTenantPolicy write, not a read-modify-write, mirrors the REST
// endpoint's own documented behavior; see internal/api/admin.go's
// SetTenantPolicyInput).
func parseTenantPolicy(args []string) (tenantID string, policy domain.TenantPolicy, err error) {
	fs := flag.NewFlagSet("tenant policy", flag.ContinueOnError)
	tenantFlag := fs.String("tenant", "", "tenant id (required)")
	maxAmountFlag := fs.Int64("max-amount", 0, "max per-currency debit total for a single transaction, in minor units (0 = unlimited, and clears any existing limit)")
	dailyLimitFlag := fs.Int64("daily-limit", 0, "max per-currency cumulative debit total for the day, in minor units (0 = unlimited, and clears any existing limit)")
	currenciesFlag := fs.String("currencies", "", "comma-separated allowed currencies, e.g. USD,EUR (omitted = every currency allowed, and clears any existing allowlist)")
	if err := fs.Parse(args); err != nil {
		return "", domain.TenantPolicy{}, err
	}
	if *tenantFlag == "" {
		return "", domain.TenantPolicy{}, errors.New("--tenant is required")
	}
	var currencies []string
	if *currenciesFlag != "" {
		for _, c := range strings.Split(*currenciesFlag, ",") {
			c = strings.ToUpper(strings.TrimSpace(c))
			if c == "" {
				continue
			}
			currencies = append(currencies, c)
		}
	}
	return *tenantFlag, domain.TenantPolicy{
		MaxTransactionAmount: *maxAmountFlag,
		DailyVolumeLimit:     *dailyLimitFlag,
		AllowedCurrencies:    currencies,
	}, nil
}

func tenantPolicySet(ctx context.Context, svc *admin.Service, out *os.File, args []string) error {
	tenantID, policy, err := parseTenantPolicy(args)
	if err != nil {
		return err
	}
	if err := svc.SetTenantPolicy(ctx, tenantID, policy); err != nil {
		return fmt.Errorf("set tenant policy: %w", err)
	}
	currencies := "any"
	if len(policy.AllowedCurrencies) > 0 {
		currencies = strings.Join(policy.AllowedCurrencies, ",")
	}
	_, _ = fmt.Fprintf(out, "tenant %s policy: max_amount=%d daily_limit=%d currencies=%s\n",
		tenantID, policy.MaxTransactionAmount, policy.DailyVolumeLimit, currencies)
	return nil
}

// --- key subcommands ---

// parseScopes splits a comma-separated --scopes value into []domain.Scope.
// It does not validate each element against Scope.Valid(): admin.Service
// does that itself (admin.ErrInvalidScopes) and this CLI just surfaces
// whatever error comes back, the same as every other admin.Service error.
func parseScopes(raw string) []domain.Scope {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]domain.Scope, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, domain.Scope(p))
	}
	return out
}

// parseKeyIssue parses "key issue"'s flags, including the optional
// --expires-in duration (e.g. "24h", "720h" for 30 days), returned as an
// absolute *time.Time relative to now. now is injected so this is testable
// without real time passing.
func parseKeyIssue(args []string, now time.Time) (tenantID, name string, scopes []domain.Scope, expiresAt *time.Time, err error) {
	fs := flag.NewFlagSet("key issue", flag.ContinueOnError)
	tenantFlag := fs.String("tenant", "", "tenant id (required)")
	nameFlag := fs.String("name", "", "human-readable key label (required)")
	scopesFlag := fs.String("scopes", "", "comma-separated: read,post,admin (required)")
	expiresInFlag := fs.String("expires-in", "", "optional duration, e.g. 24h, 720h (never expires if omitted)")
	if err := fs.Parse(args); err != nil {
		return "", "", nil, nil, err
	}
	if *tenantFlag == "" {
		return "", "", nil, nil, errors.New("--tenant is required")
	}
	if *nameFlag == "" {
		return "", "", nil, nil, errors.New("--name is required")
	}
	if *scopesFlag == "" {
		return "", "", nil, nil, errors.New("--scopes is required")
	}
	if *expiresInFlag != "" {
		d, perr := time.ParseDuration(*expiresInFlag)
		if perr != nil {
			return "", "", nil, nil, fmt.Errorf("--expires-in: %w", perr)
		}
		t := now.Add(d)
		expiresAt = &t
	}
	return *tenantFlag, *nameFlag, parseScopes(*scopesFlag), expiresAt, nil
}

func keyIssue(ctx context.Context, svc *admin.Service, out *os.File, args []string) error {
	tenantID, name, scopes, expiresAt, err := parseKeyIssue(args, time.Now())
	if err != nil {
		return err
	}
	plaintext, key, err := svc.IssueKey(ctx, tenantID, name, scopes, expiresAt)
	if err != nil {
		return fmt.Errorf("issue key: %w", err)
	}
	printIssuedKey(out, plaintext, key)
	return nil
}

// parseKeyID parses the single --id flag shared by "key rotate" and
// "key revoke".
func parseKeyID(fsName string, args []string) (string, error) {
	fs := flag.NewFlagSet(fsName, flag.ContinueOnError)
	idFlag := fs.String("id", "", "key id (required)")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *idFlag == "" {
		return "", errors.New("--id is required")
	}
	return *idFlag, nil
}

func keyRotate(ctx context.Context, svc *admin.Service, out *os.File, args []string) error {
	id, err := parseKeyID("key rotate", args)
	if err != nil {
		return err
	}
	plaintext, key, err := svc.RotateKey(ctx, id)
	if err != nil {
		return fmt.Errorf("rotate key: %w", err)
	}
	printIssuedKey(out, plaintext, key)
	_, _ = fmt.Fprintf(out, "the old key (%s) is still active; revoke it explicitly once callers have cut over\n", id)
	return nil
}

func keyRevoke(ctx context.Context, svc *admin.Service, out *os.File, args []string) error {
	id, err := parseKeyID("key revoke", args)
	if err != nil {
		return err
	}
	if err := svc.RevokeKey(ctx, id); err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	_, _ = fmt.Fprintf(out, "revoked key %s\n", id)
	return nil
}

func keyList(ctx context.Context, svc *admin.Service, out *os.File, args []string) error {
	fs := flag.NewFlagSet("key list", flag.ContinueOnError)
	tenantFlag := fs.String("tenant", "", "tenant id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantFlag == "" {
		return errors.New("--tenant is required")
	}

	keys, err := svc.ListKeys(ctx, *tenantFlag)
	if err != nil {
		return fmt.Errorf("list keys: %w", err)
	}
	if len(keys) == 0 {
		_, _ = fmt.Fprintln(out, "no keys")
		return nil
	}
	for _, k := range keys {
		status := "live"
		if k.RevokedAt != nil {
			status = "revoked"
		}
		expires := "never"
		if k.ExpiresAt != nil {
			expires = k.ExpiresAt.Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\tscopes=%s\texpires=%s\n", k.ID, status, k.Name, joinScopes(k.Scopes), expires)
	}
	return nil
}

// --- rate subcommands ---

// defaultRateSource is the fx_rates.source value a "rate set" call writes
// when --source is omitted, distinguishing an operator's manual tenant rate
// from the "env" source internal/fx.Seed writes for the global default.
const defaultRateSource = "manual"

// rateSetArgs is the parsed, validated result of "rate set"'s flags, kept
// separate from parseRateSet's flag.FlagSet plumbing so rateSet's own body
// stays a plain call into svc.SetFXRate.
type rateSetArgs struct {
	tenantID    string
	base, quote domain.Currency
	midRateE8   int64
	spreadBps   int32
	source      string
	// effectiveAt is nil when --effective-at is omitted: "effective
	// immediately" is a nil value all the way down to the SQL insert, so the
	// DATABASE SERVER's clock stamps the row (COALESCE(..., now())), never
	// this CLI process's own time.Now() (Task 2.4 remediation: a CLI host
	// even slightly ahead of the database server made a just-inserted
	// "immediate" rate transiently invisible to CurrentFXRate's "effective_at
	// <= now()" gate, which runs on the server's clock). A non-nil
	// effectiveAt (--effective-at was given) is passed through unchanged,
	// including a future timestamp for a genuinely scheduled rate.
	effectiveAt *time.Time
}

// parseRateSet parses "rate set"'s flags, exactly like parseKeyIssue does for
// "key issue": every check that does not need a database round trip runs
// here, before rateSet ever calls svc. --mid is parsed with fx.ParseRateE8,
// the same string-scaling parser internal/fx/seed.go uses for FX_RATES, so a
// decimal rate never passes through strconv.ParseFloat on its way into
// mid_rate_e8 (go-ledger's money-safety rule: integer only on the rate
// path). Unlike an earlier version of this function, there is no injected
// "now": the omitted-flag default is nil (server-stamped), not any
// particular instant, so there is nothing to inject for a test to control.
func parseRateSet(args []string) (rateSetArgs, error) {
	fs := flag.NewFlagSet("rate set", flag.ContinueOnError)
	tenantFlag := fs.String("tenant", "", "tenant id (required)")
	baseFlag := fs.String("base", "", "base currency, e.g. USD (required)")
	quoteFlag := fs.String("quote", "", "quote currency, e.g. EUR (required)")
	midFlag := fs.String("mid", "", "mid rate as a plain decimal, e.g. 0.9200 (required)")
	spreadFlag := fs.Int("spread-bps", 0, "spread in basis points, 0-9999 (default 0)")
	sourceFlag := fs.String("source", defaultRateSource, "provenance recorded on the row (default manual)")
	effectiveAtFlag := fs.String("effective-at", "", "RFC3339 timestamp (default now)")
	if err := fs.Parse(args); err != nil {
		return rateSetArgs{}, err
	}

	if *tenantFlag == "" {
		return rateSetArgs{}, errors.New("--tenant is required")
	}
	base := domain.Currency(strings.ToUpper(strings.TrimSpace(*baseFlag)))
	if err := base.Validate(); err != nil {
		return rateSetArgs{}, fmt.Errorf("--base: %w", err)
	}
	quote := domain.Currency(strings.ToUpper(strings.TrimSpace(*quoteFlag)))
	if err := quote.Validate(); err != nil {
		return rateSetArgs{}, fmt.Errorf("--quote: %w", err)
	}
	if base == quote {
		return rateSetArgs{}, domain.ErrSameCurrencyRate
	}
	if *midFlag == "" {
		return rateSetArgs{}, errors.New("--mid is required")
	}
	midRateE8, err := fx.ParseRateE8(*midFlag)
	if err != nil {
		return rateSetArgs{}, fmt.Errorf("--mid: %w", err)
	}
	if *spreadFlag < 0 || *spreadFlag >= 10_000 {
		return rateSetArgs{}, domain.ErrInvalidSpread
	}

	var effectiveAt *time.Time
	if *effectiveAtFlag != "" {
		t, err := time.Parse(time.RFC3339, *effectiveAtFlag)
		if err != nil {
			return rateSetArgs{}, fmt.Errorf("--effective-at: %w", err)
		}
		effectiveAt = &t
	}

	return rateSetArgs{
		tenantID:    *tenantFlag,
		base:        base,
		quote:       quote,
		midRateE8:   midRateE8,
		spreadBps:   int32(*spreadFlag), //nolint:gosec // bounded to [0, 10000) just above
		source:      *sourceFlag,
		effectiveAt: effectiveAt,
	}, nil
}

func rateSet(ctx context.Context, svc *admin.Service, out *os.File, args []string) error {
	a, err := parseRateSet(args)
	if err != nil {
		return err
	}
	if err := svc.SetFXRate(ctx, a.tenantID, a.base, a.quote, a.midRateE8, a.spreadBps, a.source, a.effectiveAt); err != nil {
		return fmt.Errorf("set fx rate: %w", err)
	}
	// effective_at is only known here if the operator gave --effective-at
	// explicitly; the omitted case is stamped by the database server at
	// insert time (see rateSetArgs.effectiveAt), so this prints "now (server
	// clock)" rather than guessing a timestamp this process never computed.
	effectiveAtStr := "now (server clock)"
	if a.effectiveAt != nil {
		effectiveAtStr = a.effectiveAt.Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(out, "tenant %s: %s/%s mid_rate_e8=%d spread_bps=%d source=%s effective_at=%s\n",
		a.tenantID, a.base, a.quote, a.midRateE8, a.spreadBps, a.source, effectiveAtStr)
	return nil
}

// printIssuedKey prints a newly issued or rotated key's plaintext prominently
// with a "store this now" note, plus its metadata. This is the one place in
// this codebase that deliberately prints a credential: it is never logged,
// and the plaintext is never persisted anywhere, so this stdout line is the
// only chance to capture it.
func printIssuedKey(out *os.File, plaintext string, key domain.APIKey) {
	_, _ = fmt.Fprintln(out, "=============================================================")
	_, _ = fmt.Fprintln(out, "  API KEY (shown once, store this now, it cannot be shown again)")
	_, _ = fmt.Fprintln(out, "=============================================================")
	_, _ = fmt.Fprintf(out, "  %s\n", plaintext)
	_, _ = fmt.Fprintln(out, "=============================================================")
	_, _ = fmt.Fprintf(out, "id=%s tenant=%s name=%q scopes=%s expires=%s\n",
		key.ID, key.TenantID, key.Name, joinScopes(key.Scopes), formatExpiry(key.ExpiresAt))
}

func joinScopes(scopes []domain.Scope) string {
	strs := make([]string, len(scopes))
	for i, s := range scopes {
		strs[i] = string(s)
	}
	return strings.Join(strs, ",")
}

func formatExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Format(time.RFC3339)
}
