package migrate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/config"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

const embeddedResetSchema = config.DefaultSchema

// EmbeddedResetPlan is the non-mutating view shown before an embedded reset.
type EmbeddedResetPlan struct {
	Target       string   `json:"target"`
	SchemaExists bool     `json:"schema_exists"`
	LedgerExists bool     `json:"ledger_exists"`
	LedgerRows   []string `json:"ledger_rows"`
}

// EmbeddedResetResult reports the exact target and ledger rows removed.
type EmbeddedResetResult struct {
	Target            string
	DeletedLedgerRows int64
}

// EmbeddedResetConfirmation returns the exact typed token for a reset target.
func EmbeddedResetConfirmation(target string) string {
	return "reset-openrails@" + target
}

// PlanEmbeddedReset connects to the DSN and reports the exact reset scope
// without mutating the OpenRails schema or ledger rows.
func PlanEmbeddedReset(ctx context.Context, dsn string) (plan EmbeddedResetPlan, err error) {
	conn, target, err := connectResetTarget(ctx, dsn)
	if err != nil {
		return plan, err
	}
	defer func() { err = errors.Join(err, conn.Close(ctx)) }()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return plan, fmt.Errorf("begin read-only embedded reset plan: %w", err)
	}
	plan, err = inspectEmbeddedReset(ctx, tx, target)
	if err != nil {
		return plan, errors.Join(err, tx.Rollback(ctx))
	}
	if err = tx.Commit(ctx); err != nil {
		return plan, fmt.Errorf("commit read-only embedded reset plan: %w", err)
	}
	return plan, nil
}

// ApplyEmbeddedReset drops only the default OpenRails schema and deletes only
// its exact migratekit ledger scope, in one transaction.
func ApplyEmbeddedReset(ctx context.Context, dsn, allowedTargets, confirmation string) (result EmbeddedResetResult, err error) {
	conn, target, err := connectResetTarget(ctx, dsn)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, conn.Close(ctx)) }()
	if err := validateResetAuthorization(target, allowedTargets, confirmation); err != nil {
		return result, err
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("begin embedded reset: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback(ctx))
		}
	}()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, target); err != nil {
		return result, fmt.Errorf("lock embedded reset target: %w", err)
	}
	// A default namespace is not exclusive ownership. Refuse a shared schema
	// rather than cascading through another application's tables or queue.
	var shared bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=$1 AND c.relkind IN ('r','p','v','m','f') AND NOT c.relname=ANY($2::text[]))`, embeddedResetSchema, postgresmigrations.OwnedTables).Scan(&shared); err != nil {
		return result, err
	}
	if shared {
		return result, errors.New("embedded reset refuses a shared schema containing host-owned relations")
	}
	if _, err = tx.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{embeddedResetSchema}.Sanitize()+" CASCADE"); err != nil {
		return result, fmt.Errorf("drop openrails schema: %w", err)
	}

	var ledgerExists bool
	if err = tx.QueryRow(ctx, `SELECT to_regclass('public.migrations') IS NOT NULL`).Scan(&ledgerExists); err != nil {
		return result, fmt.Errorf("check migratekit ledger: %w", err)
	}
	if ledgerExists {
		command, execErr := tx.Exec(ctx,
			`DELETE FROM public.migrations
			  WHERE app = $1 AND database = 'postgres' AND schema = $2`,
			config.MigratekitApp, embeddedResetSchema)
		if execErr != nil {
			return result, fmt.Errorf("delete openrails migration ledger: %w", execErr)
		}
		result.DeletedLedgerRows = command.RowsAffected()
	}
	if err = tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit embedded reset: %w", err)
	}
	result.Target = target
	return result, nil
}

func connectResetTarget(ctx context.Context, dsn string) (*pgx.Conn, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(dsn) == "" {
		return nil, "", fmt.Errorf("DSN is required")
	}
	connectionConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, "", fmt.Errorf("parse reset DSN: %w", err)
	}
	if err := validateResetConnectionConfig(connectionConfig); err != nil {
		return nil, "", err
	}
	target := canonicalResetTarget(connectionConfig)
	conn, err := pgx.ConnectConfig(ctx, connectionConfig)
	if err != nil {
		return nil, "", fmt.Errorf("connect to reset target %q: %w", target, err)
	}
	var database string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return nil, "", errors.Join(
			fmt.Errorf("resolve reset database: %w", err),
			conn.Close(ctx),
		)
	}
	if database != connectionConfig.Database {
		return nil, "", errors.Join(
			fmt.Errorf("reset target changed databases: parsed %q, connected to %q", connectionConfig.Database, database),
			conn.Close(ctx),
		)
	}
	return conn, target, nil
}

func validateResetConnectionConfig(connectionConfig *pgx.ConnConfig) error {
	primaryHost := strings.ToLower(strings.TrimSpace(connectionConfig.Host))
	for _, fallback := range connectionConfig.Fallbacks {
		fallbackHost := strings.ToLower(strings.TrimSpace(fallback.Host))
		if fallbackHost != primaryHost || fallback.Port != connectionConfig.Port {
			return fmt.Errorf(
				"reset DSN has multiple host targets (%s and %s); use a single exact host:port",
				net.JoinHostPort(primaryHost, strconv.Itoa(int(connectionConfig.Port))),
				net.JoinHostPort(fallbackHost, strconv.Itoa(int(fallback.Port))),
			)
		}
	}
	return nil
}

func canonicalResetTarget(connectionConfig *pgx.ConnConfig) string {
	host := strings.ToLower(strings.TrimSpace(connectionConfig.Host))
	address := net.JoinHostPort(host, strconv.Itoa(int(connectionConfig.Port)))
	return address + "/" + url.PathEscape(connectionConfig.Database)
}

type resetQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func inspectEmbeddedReset(ctx context.Context, conn resetQuerier, target string) (EmbeddedResetPlan, error) {
	plan := EmbeddedResetPlan{Target: target, LedgerRows: []string{}}
	if err := conn.QueryRow(ctx, `SELECT to_regnamespace($1) IS NOT NULL`, embeddedResetSchema).
		Scan(&plan.SchemaExists); err != nil {
		return plan, fmt.Errorf("check openrails schema: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.migrations') IS NOT NULL`).
		Scan(&plan.LedgerExists); err != nil {
		return plan, fmt.Errorf("check migratekit ledger: %w", err)
	}
	if !plan.LedgerExists {
		return plan, nil
	}
	rows, err := conn.Query(ctx,
		`SELECT sequence FROM public.migrations
		  WHERE app = $1 AND database = 'postgres' AND schema = $2
		  ORDER BY sequence`,
		config.MigratekitApp, embeddedResetSchema)
	if err != nil {
		return plan, fmt.Errorf("read openrails migration ledger: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sequence int64
		if err := rows.Scan(&sequence); err != nil {
			return plan, fmt.Errorf("scan openrails migration ledger: %w", err)
		}
		plan.LedgerRows = append(plan.LedgerRows, strconv.FormatInt(sequence, 10))
	}
	if err := rows.Err(); err != nil {
		return plan, fmt.Errorf("iterate openrails migration ledger: %w", err)
	}
	return plan, nil
}

func validateResetAuthorization(target, allowedTargets, confirmation string) error {
	allowed := false
	for _, candidate := range strings.Split(allowedTargets, ",") {
		if strings.TrimSpace(candidate) == target {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("reset target %q is not allow-listed in OPENRAILS_RESET_TARGETS", target)
	}
	expected := EmbeddedResetConfirmation(target)
	if confirmation != expected {
		return fmt.Errorf("reset confirmation does not match target %q; expected %q", target, expected)
	}
	return nil
}

// Report renders the plan without exposing DSN credentials.
func (p EmbeddedResetPlan) Report() string {
	var out strings.Builder
	fmt.Fprintf(&out, "embedded reset target: %s\n", p.Target)
	fmt.Fprintf(&out, "openrails schema exists: %t\n", p.SchemaExists)
	fmt.Fprintf(&out, "migratekit ledger exists: %t\n", p.LedgerExists)
	out.WriteString("openrails migration ledger rows:\n")
	if len(p.LedgerRows) == 0 {
		out.WriteString("  none\n")
	} else {
		for _, name := range p.LedgerRows {
			fmt.Fprintf(&out, "  %s\n", name)
		}
	}
	out.WriteString("plan (one transaction):\n")
	fmt.Fprintf(&out, "  DROP SCHEMA IF EXISTS %s CASCADE;\n", embeddedResetSchema)
	fmt.Fprintf(&out, "  DELETE FROM public.migrations WHERE app = 'openrails' AND database = 'postgres' AND schema = '%s';\n", embeddedResetSchema)
	fmt.Fprintf(&out, "allow-list entry: %s\n", p.Target)
	fmt.Fprintf(&out, "confirmation token: %s\n", EmbeddedResetConfirmation(p.Target))
	return out.String()
}
