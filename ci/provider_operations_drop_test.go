//go:build e2e && integration

package ci_test

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

// The migration that deletes provider operations refuses to run while a hold
// is open, leaving every table in place, and runs once the hold is released:
// the six tables go and their open findings resolve.
func TestProviderOperationTablesDropRefusesAnOpenHold(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	require.NotEmpty(t, dsn, "OPENRAILS_E2E_DSN must point at a disposable PostgreSQL database")
	pool := openPool(t, dsn)
	f := &fixture{pool: pool, schema: "e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
	})
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	before := fstest.MapFS{}
	for _, name := range files {
		if strings.HasSuffix(name, "_drop_provider_operations.up.sql") {
			break
		}
		body, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		before[name] = &fstest.MapFile{Data: body}
	}
	require.Less(t, len(before), len(files), "the migration is in this build's chain")
	cfg := f.config()
	cfg.DB = &openrails.DBConfig{URL: dsn}
	application, err := app.BootstrapWithOptions(t.Context(), &cfg, &app.BootstrapOptions{PGXPool: pool, Migrations: before})
	require.NoError(t, err)
	require.NoError(t, application.Close(t.Context()))

	ctx := t.Context()
	table := func(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	var merchantID, account uuid.UUID
	customerID := uuid.New()
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO `+table("merchants")+` (slug, status) VALUES ($1, 'active') RETURNING id`,
		"rentals-"+uuid.NewString()[:8]).Scan(&merchantID))
	exec(`INSERT INTO `+table("customers")+` (merchant_id, id) VALUES ($1, $2)`, merchantID, customerID)
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO `+table("ledger_accounts")+`
		(merchant_id, customer_id, account_type, currency, debits_must_not_exceed_credits, credits_must_not_exceed_debits)
		VALUES ($1, $2, 'customer_balance', 'USD', true, false) RETURNING id`, merchantID, customerID).Scan(&account))
	exec(`INSERT INTO `+table("operation_authorizations")+`
		(operation_id, merchant_id, customer_id, record_owner, ledger_account_id, currency, amount, claim_reference,
		 authorization_body_bytes, authorization_body_digest)
		VALUES ('rental/1', $1, $2, 'host', $3, 'USD', 1000000, 'claim-1', 'body'::bytea, sha256('body'::bytea))`, merchantID, customerID, account)
	exec(`INSERT INTO `+table("reconciliation_findings")+` (merchant_id, finding_type, subject_key, severity, status)
		VALUES ($1, 'life.provider_operation.silent', 'provider_operation:rental/1', 'high', 'requires_review')`, merchantID)

	exists := func(name string) bool {
		var found *string
		require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass($1)::text", table(name)).Scan(&found))
		return found != nil
	}
	dropped := []string{"operation_authorizations", "operation_authorization_extensions",
		"cost_qualifications", "cost_observations", "cost_refusals", "cost_resolutions"}

	_, err = openrails.New(ctx, f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.ErrorContains(t, err, "open authorizations", "an open hold is a customer's held money")
	for _, name := range dropped {
		require.True(t, exists(name), "%s stays while the hold is open", name)
	}

	exec(`UPDATE `+table("operation_authorizations")+` SET state = 'released', terminal_reference = 'host-release', released_at = now()
		WHERE merchant_id = $1 AND operation_id = 'rental/1'`, merchantID)
	bill, err := openrails.New(ctx, f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.NoError(t, err, "a released hold lets the upgrade run")
	require.NoError(t, bill.Close(ctx))
	for _, name := range dropped {
		require.False(t, exists(name), name)
	}
	var status, resolution string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status, resolution FROM `+table("reconciliation_findings")+`
		WHERE merchant_id = $1 AND finding_type = 'life.provider_operation.silent'`, merchantID).Scan(&status, &resolution))
	require.Equal(t, [2]string{"fixed", "auto_vanished"}, [2]string{status, resolution})
}
