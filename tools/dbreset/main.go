package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/open-rails/openrails/internal/migrate"
)

func main() {
	if err := run(context.Background(), os.Stdout, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "db-reset-embedded: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out io.Writer, getenv func(string) string) error {
	dsn := strings.TrimSpace(getenv("OPENRAILS_RESET_DSN"))
	plan, err := migrate.PlanEmbeddedReset(ctx, dsn)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(out, plan.Report()); err != nil {
		return fmt.Errorf("write embedded reset plan: %w", err)
	}
	confirmation := getenv("OPENRAILS_RESET_CONFIRMATION")
	if confirmation == "" {
		_, err := fmt.Fprintln(out, "plan only: stop the embedded host, then rerun with the exact allow-list entry and confirmation token shown above")
		return err
	}
	result, err := migrate.ApplyEmbeddedReset(ctx, dsn, getenv("OPENRAILS_RESET_TARGETS"), confirmation)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "reset complete: target=%s deleted_ledger_rows=%d\n",
		result.Target, result.DeletedLedgerRows)
	return err
}
