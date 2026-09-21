//go:build integration

package postgresmigrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestOwningLoginInitializesWithoutRLSAndPreservesFinancialFacts(t *testing.T) {
	ctx := t.Context()
	adminConfig, err := pgxpool.ParseConfig(dbtest.SharedSuperuserDSN(t))
	require.NoError(t, err)
	adminConfig.ConnConfig.Database = "postgres"
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	name := "owner_integrity_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	role := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD 'owner_test'")
	require.NoError(t, err)
	t.Cleanup(func() { _, err := admin.Exec(context.Background(), "DROP ROLE "+role); require.NoError(t, err) })
	_, err = admin.Exec(ctx, "CREATE DATABASE "+role+" OWNER "+role)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(), "DROP DATABASE "+role+" WITH (FORCE)")
		require.NoError(t, err)
	})
	ownerConfig := adminConfig.Copy()
	ownerConfig.ConnConfig.Database = name
	ownerConfig.ConnConfig.User = name
	ownerConfig.ConnConfig.Password = "owner_test"
	ownerConfig.MaxConns = 4
	owner, err := pgxpool.NewWithConfig(ctx, ownerConfig)
	require.NoError(t, err)
	t.Cleanup(owner.Close)
	require.NoError(t, migrate.ApplyPostgresMigrations(ctx, owner, migrate.Options{Schema: "owned_billing"}))
	require.NoError(t, migrate.ApplyPostgresMigrations(ctx, owner, migrate.Options{Schema: "owned_billing"}))
	var flags bool
	require.NoError(t, owner.QueryRow(ctx, "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&flags))
	require.False(t, flags)
	require.NoError(t, owner.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_class WHERE relnamespace='owned_billing'::regnamespace AND (relrowsecurity OR relforcerowsecurity)) OR EXISTS(SELECT 1 FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid WHERE c.relnamespace='owned_billing'::regnamespace)").Scan(&flags))
	require.False(t, flags)
	mid, other, debit, credit := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	_, err = owner.Exec(ctx, "INSERT INTO owned_billing.merchants(id,slug) VALUES($1,'owner-a'),($2,'owner-b')", mid, other)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, "INSERT INTO owned_billing.ledger_accounts(id,merchant_id,account_type,currency) VALUES($1,$3,'world','USD'),($2,$3,'processor_clearing','USD')", debit, credit, mid)
	require.NoError(t, err)
	transferSQL := `INSERT INTO owned_billing.ledger_transfers(merchant_id,debit_account_id,credit_account_id,amount,currency,transfer_type,operation,source,source_id) VALUES($1,$2,$3,3,'USD','deposit','deposit','owner-proof',$4),($1,$2,$3,7,'USD','deposit','deposit','owner-proof',$5)`
	_, err = owner.Exec(ctx, transferSQL, mid, debit, credit, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	var concurrent errgroup.Group
	for range 4 {
		concurrent.Go(func() error {
			_, err := owner.Exec(ctx, transferSQL, mid, debit, credit, uuid.NewString(), uuid.NewString())
			return err
		})
	}
	require.NoError(t, concurrent.Wait())
	var debits, credits int64
	require.NoError(t, owner.QueryRow(ctx, "SELECT debits_posted FROM owned_billing.ledger_accounts WHERE merchant_id=$1 AND id=$2", mid, debit).Scan(&debits))
	require.EqualValues(t, 50, debits)
	require.NoError(t, owner.QueryRow(ctx, "SELECT credits_posted FROM owned_billing.ledger_accounts WHERE merchant_id=$1 AND id=$2", mid, credit).Scan(&credits))
	require.EqualValues(t, 50, credits)
	for _, sql := range []string{
		"UPDATE owned_billing.ledger_accounts SET credits_posted=credits_posted+1 WHERE merchant_id=$1",
		"DELETE FROM owned_billing.ledger_accounts WHERE merchant_id=$1",
		"UPDATE owned_billing.ledger_transfers SET amount=amount+1 WHERE merchant_id=$1",
		"DELETE FROM owned_billing.ledger_transfers WHERE merchant_id=$1",
	} {
		_, err := owner.Exec(ctx, sql, mid)
		require.ErrorContains(t, err, "immutable")
	}
	_, err = owner.Exec(ctx, "TRUNCATE owned_billing.ledger_transfers CASCADE")
	require.ErrorContains(t, err, "immutable")
	_, err = owner.Exec(ctx, "INSERT INTO owned_billing.maintenance_runs(merchant_id,kind,actor,mode) VALUES($1,'reconciliation','owner-test','advisory')", mid)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, "UPDATE owned_billing.maintenance_runs SET actor='forged' WHERE merchant_id=$1", mid)
	require.ErrorContains(t, err, "immutable")
	_, err = owner.Exec(ctx, "UPDATE owned_billing.maintenance_runs SET status='completed',finished_at=now() WHERE merchant_id=$1", mid)
	require.NoError(t, err)
	// Explicit global entrypoints no longer need a privileged definer.
	var count int64
	require.NoError(t, owner.QueryRow(ctx, "SELECT owned_billing.count_destructive_intents_by_actor_since($1,$2,now()-interval '1 day')", "owner-test", []string{"nmi_refund"}).Scan(&count))
	require.Zero(t, count)
	// Empty-book restore is serialized and validated equally for an owning login.
	tx, err := owner.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "SELECT set_config('app.merchant_id',$1,true)", other.String())
	require.NoError(t, err)
	var receipt uuid.UUID
	require.NoError(t, tx.QueryRow(ctx, "SELECT owned_billing.begin_billing_restore($1)", other).Scan(&receipt))
	_, err = tx.Exec(ctx, "SELECT owned_billing.finish_billing_restore($1,$2,0)", other, strings.Repeat("a", 64))
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	_, err = owner.Exec(ctx, "UPDATE owned_billing.maintenance_runs SET summary='{}' WHERE merchant_id=$1", other)
	require.Error(t, err)
	_, err = owner.Exec(ctx, "DELETE FROM owned_billing.maintenance_runs WHERE merchant_id=$1", other)
	require.ErrorContains(t, err, "immutable")
}
