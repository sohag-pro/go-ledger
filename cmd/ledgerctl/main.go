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
	},
	"key": {
		"issue":  keyIssue,
		"rotate": keyRotate,
		"revoke": keyRevoke,
		"list":   keyList,
	},
}

// usage is printed on a missing or unrecognized subcommand.
const usage = `usage: ledgerctl <resource> <action> [flags]

  tenant create --name NAME
  tenant list
  tenant status --id ID --status active|suspended|closed

  key issue --tenant ID --name NAME --scopes read,post[,admin] [--expires-in DURATION]
  key rotate --id KEYID
  key revoke --id KEYID
  key list --tenant ID

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
