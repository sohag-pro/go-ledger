// Interactive first-run Postgres setup (ADR-019, "Interactive first-run
// setup for the binary"). Strictly TTY-gated: this code path only ever runs
// when DATABASE_URL is unset AND stdin is a terminal (see isInteractive and
// loadConfigWithTTY in main.go). Every other environment, docker, systemd,
// CI, or a plain "go run" with input piped in, keeps today's fail-fast
// behavior unchanged.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

// DBParts are the Postgres connection fields collected interactively.
type DBParts struct{ Host, Port, DB, User, Password, SSLMode string }

// buildDatabaseURL assembles a libpq/pgx connection URL, percent-encoding the
// userinfo so a password with special characters (an "@" or "/", say) cannot
// corrupt the URL's own delimiters.
func buildDatabaseURL(p DBParts) string {
	u := &url.URL{Scheme: "postgres", Host: p.Host + ":" + p.Port, Path: "/" + p.DB}
	u.User = url.UserPassword(p.User, p.Password)
	q := u.Query()
	q.Set("sslmode", p.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// isInteractive reports whether stdin is a terminal. This is the single
// gate for the entire interactive setup flow (ADR-019): loadConfig calls
// this directly, while loadConfigWithTTY takes the result as a parameter so
// tests can force both branches without a real terminal.
func isInteractive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// prompt reads one line with a default: an empty line (just pressing enter)
// takes def.
func prompt(r *bufio.Reader, out io.Writer, label, def string) string {
	if def != "" {
		_, _ = fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		_, _ = fmt.Fprintf(out, "%s: ", label)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// runInteractiveSetup collects a DATABASE_URL (a full URL, or individual
// parts), tests it with ping, and reports whether to save it and where. It
// is pure aside from stdin/out and the two injected functions (readPassword,
// ping), so it is fully testable with fakes: no real terminal IO and no
// real database connection.
func runInteractiveSetup(in io.Reader, out io.Writer, readPassword func() (string, error), ping func(string) error) (dbURL string, save bool, savePath string, err error) {
	r := bufio.NewReader(in)
	_, _ = fmt.Fprintln(out, "No DATABASE_URL set. Let's configure the Postgres connection.")
	full := prompt(r, out, "Full DATABASE_URL (leave blank to enter parts)", "")
	if full != "" {
		dbURL = full
	} else {
		p := DBParts{
			Host:    prompt(r, out, "Postgres host", "localhost"),
			Port:    prompt(r, out, "Port", "5432"),
			DB:      prompt(r, out, "Database name", "ledger"),
			User:    prompt(r, out, "User", "ledger"),
			SSLMode: prompt(r, out, "sslmode", "disable"),
		}
		_, _ = fmt.Fprint(out, "Password: ")
		p.Password, err = readPassword()
		_, _ = fmt.Fprintln(out)
		if err != nil {
			return "", false, "", fmt.Errorf("read password: %w", err)
		}
		dbURL = buildDatabaseURL(p)
	}
	if err = ping(dbURL); err != nil {
		return "", false, "", fmt.Errorf("could not connect: %w", err)
	}
	_, _ = fmt.Fprintln(out, "Connection OK.")
	if strings.EqualFold(prompt(r, out, "Save this to a config file for next time? (y/N)", "N"), "y") {
		save = true
		savePath = prompt(r, out, "Config file path", "./.env")
	}
	return dbURL, save, savePath, nil
}

// pingDB opens a short-lived pgxpool against url, pings it, and closes it.
// This is the real ping runInteractiveSetup is wired to from
// loadConfigWithTTY; a test supplies a fake instead so no real database is
// needed to exercise the flow.
func pingDB(dbURL string) error {
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return pool.Ping(ctx)
}

// readPasswordFromStdin reads a hidden password from the real terminal
// (golang.org/x/term.ReadPassword), the real readPassword implementation
// runInteractiveSetup is wired to from loadConfigWithTTY.
func readPasswordFromStdin() (string, error) {
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// saveDatabaseURL writes DATABASE_URL=<url> to path, mode 0600, so a config
// file created by the interactive flow never lands with a connection string
// (which may contain a plaintext password) world- or group-readable.
func saveDatabaseURL(path, dbURL string) error {
	return os.WriteFile(path, []byte("DATABASE_URL="+dbURL+"\n"), 0o600)
}
