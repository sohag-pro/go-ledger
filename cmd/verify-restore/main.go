// Command verify-restore checks a restored ledger database against the
// ledger's own invariants (ADR-016, "Automated restore-and-verify"). It is
// meant to run against a throwaway database that a CI job has just restored
// from the offsite pgBackRest repository, never against production: a backup
// is not trusted until a restore is proven.
//
// It reads DATABASE_URL, runs internal/verify.Run, logs a structured summary,
// and exits non-zero whenever the restored ledger fails its invariants or an
// infrastructure error prevents the check from running at all.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/verify"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("restore verification failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	report, err := verify.Run(ctx, pool)
	if err != nil {
		return err
	}

	logAttrs := []any{
		"tenants_checked", report.TenantsChecked,
		"balance_violations", len(report.BalanceViolations),
		"chain_breaks", len(report.ChainBreaks),
		"ok", report.OK(),
	}
	if report.OK() {
		logger.Info("restore verification passed", logAttrs...)
		return nil
	}

	for _, v := range report.BalanceViolations {
		logger.Error("balance violation",
			"transaction_id", v.TransactionID, "currency", v.Currency, "sum", v.Sum)
	}
	for _, b := range report.ChainBreaks {
		logger.Error("audit chain break",
			"tenant_id", b.TenantID, "first_break_id", b.FirstBreakID, "checked", b.Checked)
	}
	logger.Error("restore verification failed invariants", logAttrs...)
	return errors.New("restored ledger failed invariant verification")
}
