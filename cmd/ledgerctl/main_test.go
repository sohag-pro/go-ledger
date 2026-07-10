package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/domain"
)

// --- resolveHandler: subcommand dispatch, no serviceFactory involved at all. ---

func TestResolveHandlerKnownSubcommands(t *testing.T) {
	t.Parallel()
	cases := []struct{ resource, action string }{
		{"tenant", "create"},
		{"tenant", "list"},
		{"tenant", "status"},
		{"key", "issue"},
		{"key", "rotate"},
		{"key", "revoke"},
		{"key", "list"},
	}
	for _, tc := range cases {
		h, err := resolveHandler(tc.resource, tc.action)
		if err != nil {
			t.Errorf("resolveHandler(%q, %q): %v", tc.resource, tc.action, err)
		}
		if h == nil {
			t.Errorf("resolveHandler(%q, %q) returned a nil handler", tc.resource, tc.action)
		}
	}
}

func TestResolveHandlerUnknownResource(t *testing.T) {
	t.Parallel()
	_, err := resolveHandler("account", "create")
	if err == nil {
		t.Fatal("expected an error for an unknown resource")
	}
	if !strings.Contains(err.Error(), `unknown resource "account"`) {
		t.Errorf("error = %q, want it to name the unknown resource", err.Error())
	}
}

func TestResolveHandlerUnknownAction(t *testing.T) {
	t.Parallel()
	_, err := resolveHandler("tenant", "delete")
	if err == nil {
		t.Fatal("expected an error for an unknown action")
	}
	if !strings.Contains(err.Error(), `unknown action "delete"`) {
		t.Errorf("error = %q, want it to name the unknown action", err.Error())
	}
}

// --- run: dispatch happens before serviceFactory is ever called. ---

// TestRunUnknownSubcommandNeverCallsServiceFactory proves resolveHandler
// runs before newService (see run): a bad resource/action fails immediately,
// with no DATABASE_URL or database round trip attempted at all.
func TestRunUnknownSubcommandNeverCallsServiceFactory(t *testing.T) {
	t.Parallel()
	called := false
	factory := func(context.Context) (*admin.Service, func(), error) {
		called = true
		return nil, func() {}, nil
	}

	err := run([]string{"bogus", "action"}, os.Stdout, factory)
	if err == nil {
		t.Fatal("expected an error for an unknown resource/action pair")
	}
	if called {
		t.Error("run called the service factory for an unrecognized subcommand; it should fail before connecting")
	}
}

func TestRunTooFewArgs(t *testing.T) {
	t.Parallel()
	called := false
	factory := func(context.Context) (*admin.Service, func(), error) {
		called = true
		return nil, func() {}, nil
	}

	if err := run([]string{"tenant"}, os.Stdout, factory); err == nil {
		t.Fatal("expected a usage error with fewer than two args")
	}
	if called {
		t.Error("run called the service factory with too few args")
	}
}

// TestRunServiceFactoryErrorPropagates proves a serviceFactory failure (e.g.
// missing DATABASE_URL in the real one) surfaces as run's own error, once a
// recognized subcommand has already been resolved.
func TestRunServiceFactoryErrorPropagates(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("DATABASE_URL is required")
	factory := func(context.Context) (*admin.Service, func(), error) {
		return nil, nil, wantErr
	}

	err := run([]string{"tenant", "list"}, os.Stdout, factory)
	if !errors.Is(err, wantErr) {
		t.Errorf("run error = %v, want %v", err, wantErr)
	}
}

// --- pure flag-parsing helpers, no database or service involved. ---

func TestParseTenantStatusValid(t *testing.T) {
	t.Parallel()
	id, status, err := parseTenantStatus([]string{"--id", "tenant-1", "--status", "suspended"})
	if err != nil {
		t.Fatalf("parseTenantStatus: %v", err)
	}
	if id != "tenant-1" {
		t.Errorf("id = %q, want tenant-1", id)
	}
	if status != domain.TenantSuspended {
		t.Errorf("status = %q, want suspended", status)
	}
}

func TestParseTenantStatusMissingID(t *testing.T) {
	t.Parallel()
	_, _, err := parseTenantStatus([]string{"--status", "active"})
	if err == nil {
		t.Fatal("expected an error for a missing --id")
	}
}

func TestParseTenantStatusInvalidStatus(t *testing.T) {
	t.Parallel()
	_, _, err := parseTenantStatus([]string{"--id", "tenant-1", "--status", "pending"})
	if err == nil {
		t.Fatal("expected an error for an invalid --status")
	}
}

func TestParseScopes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want []domain.Scope
	}{
		{"empty", "", nil},
		{"single", "read", []domain.Scope{domain.ScopeRead}},
		{"multiple", "read,post", []domain.Scope{domain.ScopeRead, domain.ScopePost}},
		{"whitespace and trailing comma", " read , post ,", []domain.Scope{domain.ScopeRead, domain.ScopePost}},
		{"unknown scope passed through for the service to reject", "superuser", []domain.Scope{"superuser"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseScopes(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("parseScopes(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseScopes(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseKeyIssueRequiredFlags(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		args []string
	}{
		{"missing tenant", []string{"--name", "n", "--scopes", "read"}},
		{"missing name", []string{"--tenant", "t", "--scopes", "read"}},
		{"missing scopes", []string{"--tenant", "t", "--name", "n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, _, err := parseKeyIssue(tc.args, now)
			if err == nil {
				t.Fatalf("parseKeyIssue(%v): expected an error", tc.args)
			}
		})
	}
}

func TestParseKeyIssueExpiresIn(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tenantID, name, scopes, expiresAt, err := parseKeyIssue(
		[]string{"--tenant", "t1", "--name", "ci", "--scopes", "read,post", "--expires-in", "24h"}, now)
	if err != nil {
		t.Fatalf("parseKeyIssue: %v", err)
	}
	if tenantID != "t1" || name != "ci" {
		t.Errorf("tenantID/name = %q/%q, want t1/ci", tenantID, name)
	}
	if len(scopes) != 2 || scopes[0] != domain.ScopeRead || scopes[1] != domain.ScopePost {
		t.Errorf("scopes = %v, want [read post]", scopes)
	}
	want := now.Add(24 * time.Hour)
	if expiresAt == nil || !expiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v", expiresAt, want)
	}
}

func TestParseKeyIssueNoExpiryByDefault(t *testing.T) {
	t.Parallel()
	_, _, _, expiresAt, err := parseKeyIssue([]string{"--tenant", "t1", "--name", "ci", "--scopes", "read"}, time.Now())
	if err != nil {
		t.Fatalf("parseKeyIssue: %v", err)
	}
	if expiresAt != nil {
		t.Errorf("expiresAt = %v, want nil (never expires)", *expiresAt)
	}
}

func TestParseKeyIssueBadDuration(t *testing.T) {
	t.Parallel()
	_, _, _, _, err := parseKeyIssue([]string{"--tenant", "t1", "--name", "ci", "--scopes", "read", "--expires-in", "not-a-duration"}, time.Now())
	if err == nil {
		t.Fatal("expected an error for a malformed --expires-in")
	}
}

func TestParseKeyID(t *testing.T) {
	t.Parallel()
	id, err := parseKeyID("key rotate", []string{"--id", "key-1"})
	if err != nil {
		t.Fatalf("parseKeyID: %v", err)
	}
	if id != "key-1" {
		t.Errorf("id = %q, want key-1", id)
	}

	if _, err := parseKeyID("key rotate", nil); err == nil {
		t.Fatal("expected an error for a missing --id")
	}
}

func TestJoinAndFormatHelpers(t *testing.T) {
	t.Parallel()
	if got := joinScopes(nil); got != "" {
		t.Errorf("joinScopes(nil) = %q, want empty", got)
	}
	if got := joinScopes([]domain.Scope{domain.ScopeRead, domain.ScopePost}); got != "read,post" {
		t.Errorf("joinScopes = %q, want read,post", got)
	}
	if got := formatExpiry(nil); got != "never" {
		t.Errorf("formatExpiry(nil) = %q, want never", got)
	}
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := formatExpiry(&when); got != when.Format(time.RFC3339) {
		t.Errorf("formatExpiry = %q, want %q", got, when.Format(time.RFC3339))
	}
}
