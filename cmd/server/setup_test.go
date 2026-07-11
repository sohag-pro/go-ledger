package main

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// TestBuildDatabaseURL proves the assembled URL percent-encodes userinfo (a
// password containing "@" and "/" must not corrupt the URL's own delimiters)
// and carries every DBParts field through to the right URL component.
func TestBuildDatabaseURL(t *testing.T) {
	got := buildDatabaseURL(DBParts{Host: "localhost", Port: "5432", DB: "ledger", User: "ledger", Password: "p@ss/word", SSLMode: "disable"})
	want := "postgres://ledger:p%40ss%2Fword@localhost:5432/ledger?sslmode=disable" //nolint:gosec // test fixture, not a real credential
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestLoadConfig_NoTTY_FailsFast proves the non-interactive path (docker,
// systemd, CI: no terminal attached) is unchanged by this feature: an empty
// DATABASE_URL still fails boot immediately with the existing clear error,
// never falling into the interactive prompt flow.
func TestLoadConfig_NoTTY_FailsFast(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	_, err := loadConfigWithTTY(false)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("want fail-fast, got %v", err)
	}
}

// TestRunInteractiveSetup_FullURL proves that entering a full DATABASE_URL at
// the first prompt skips the individual-parts prompts entirely and is used
// as-is (after a successful ping).
func TestRunInteractiveSetup_FullURL(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("postgres://u:p@host:5432/db?sslmode=disable\nN\n"))
	var out strings.Builder
	var pinged string
	ping := func(url string) error {
		pinged = url
		return nil
	}
	readPassword := func() (string, error) {
		t.Fatal("readPassword should not be called when a full URL is supplied")
		return "", nil
	}

	url, save, savePath, err := runInteractiveSetup(in, &out, readPassword, ping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = "postgres://u:p@host:5432/db?sslmode=disable" //nolint:gosec // test fixture, not a real credential
	if url != want {
		t.Errorf("url = %q want %q", url, want)
	}
	if pinged != want {
		t.Errorf("ping called with %q want %q", pinged, want)
	}
	if save {
		t.Errorf("save = true, want false (answered N)")
	}
	if savePath != "" {
		t.Errorf("savePath = %q, want empty", savePath)
	}
}

// TestRunInteractiveSetup_Parts proves the parts flow assembles the same URL
// buildDatabaseURL would, using defaults for blank answers, and reports a
// save path when the operator opts in.
func TestRunInteractiveSetup_Parts(t *testing.T) {
	// Blank lines for host/port/db/user/sslmode take their defaults; "y" and
	// a custom path drive the save prompt.
	in := bufio.NewReader(strings.NewReader("\n\n\n\n\n\ny\n/etc/go-ledger/.env\n"))
	var out strings.Builder
	ping := func(_ string) error { return nil }
	readPassword := func() (string, error) { return "p@ss/word", nil }

	url, save, savePath, err := runInteractiveSetup(in, &out, readPassword, ping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = "postgres://ledger:p%40ss%2Fword@localhost:5432/ledger?sslmode=disable" //nolint:gosec // test fixture, not a real credential
	if url != want {
		t.Errorf("url = %q want %q", url, want)
	}
	if !save {
		t.Errorf("save = false, want true (answered y)")
	}
	if savePath != "/etc/go-ledger/.env" {
		t.Errorf("savePath = %q want /etc/go-ledger/.env", savePath)
	}
}

// TestRunInteractiveSetup_PingFailure proves a failed connection test is
// surfaced as an error and never asks about saving a connection that does
// not actually work.
func TestRunInteractiveSetup_PingFailure(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("postgres://u:p@host:5432/db?sslmode=disable\n"))
	var out strings.Builder
	wantErr := errors.New("connection refused")
	ping := func(_ string) error { return wantErr }
	readPassword := func() (string, error) {
		t.Fatal("readPassword should not be called for a full URL")
		return "", nil
	}

	_, save, savePath, err := runInteractiveSetup(in, &out, readPassword, ping)
	if err == nil {
		t.Fatal("want error on ping failure, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want it to wrap %v", err, wantErr)
	}
	if save {
		t.Errorf("save = true, want false on ping failure")
	}
	if savePath != "" {
		t.Errorf("savePath = %q, want empty on ping failure", savePath)
	}
}

// TestRunInteractiveSetup_ReadPasswordError proves a failure reading the
// hidden password (for example EOF on a closed stdin) short-circuits before
// ever pinging.
func TestRunInteractiveSetup_ReadPasswordError(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("\n\n\n\n\n"))
	var out strings.Builder
	wantErr := errors.New("read password failed")
	pinged := false
	ping := func(_ string) error {
		pinged = true
		return nil
	}
	readPassword := func() (string, error) { return "", wantErr }

	_, _, _, err := runInteractiveSetup(in, &out, readPassword, ping)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want it to wrap %v", err, wantErr)
	}
	if pinged {
		t.Errorf("ping was called despite a readPassword error")
	}
}
