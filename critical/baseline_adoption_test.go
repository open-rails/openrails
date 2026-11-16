//go:build integration

package critical_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// riverApart gives River a schema of its own, as a host has by default. The
// migration step compares everything in OpenRails' schema with a fresh build
// of the migrations, and the harness otherwise keeps River's tables there.
func riverApart(t *testing.T) func(*world) {
	return func(w *world) {
		river := "river_" + strings.TrimPrefix(w.schema, "critical_")
		w.cfg.Database.RiverSchema = river
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{river}.Sanitize()+" CASCADE")
			require.NoError(t, err)
		})
	}
}

// The migration ledger has no public read. These two are fixture state and
// the record of what the migration step settled, never a billing oracle.
const baselineRecordWhere = ` WHERE app = 'openrails' AND database = 'postgres' AND schema = $1 AND sequence = 1 AND filename = '0001_schema.up.sql'`

func baselineRecord(t *testing.T, w *world) [2]string {
	t.Helper()
	var content, semantic string
	require.NoError(t, w.pool.QueryRow(t.Context(), `SELECT content_sha256, semantic_sha256 FROM public.migrations`+baselineRecordWhere, w.schema).Scan(&content, &semantic))
	return [2]string{content, semantic}
}

// editBaselineRecord leaves the ledger as a database that applied a baseline
// of other statements has it.
func editBaselineRecord(t *testing.T, w *world) [2]string {
	t.Helper()
	other := [2]string{strings.Repeat("a", 64), strings.Repeat("b", 64)}
	tag, err := w.pool.Exec(t.Context(), `UPDATE public.migrations SET content_sha256 = $2, semantic_sha256 = $3`+baselineRecordWhere, w.schema, other[0], other[1])
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
	return other
}

func TestBaselineAdoption(t *testing.T) {
	t.Run("OR-J31-S12/an-older-database-whose-schema-equals-the-baseline-is-accepted-and-re-stamped", func(t *testing.T) {
		w := newWorld(t, riverApart(t))
		meterForCapture(t, w)
		customer, _ := fundedCustomer(t, w)
		admitted, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "spent", 400), reservation(w, customer, "held", 100)})
		require.NoError(t, err)
		require.True(t, admitted[0].Allowed())
		require.True(t, admitted[1].Allowed())
		spent, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("spent", 400)})
		require.NoError(t, err)
		require.NoError(t, spent[0].Err())
		require.NotNil(t, spent[0].Receipt.UsageEventID)
		balance(t, w.alpha, customer, 500, 100, 600)
		require.NoError(t, w.engine.Close(t.Context()))

		// A database a few months old: it holds a month no fresh build makes
		// and lacks one a fresh build does.
		schema := pgx.Identifier{w.schema}.Sanitize()
		var made int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT "+schema+".ensure_month_partitions('usage_events', now() - interval '6 months', now() - interval '6 months')").Scan(&made))
		require.Equal(t, 1, made)
		var old, ahead string
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT m.partition::text FROM "+schema+".month_partitions('usage_events') m ORDER BY m.range_from LIMIT 1").Scan(&old))
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT m.partition::text FROM "+schema+".month_partitions('admission_operations') m ORDER BY m.range_from DESC LIMIT 1").Scan(&ahead))
		_, err = w.pool.Exec(t.Context(), "DROP TABLE "+pgx.Identifier{w.schema, ahead}.Sanitize())
		require.NoError(t, err)

		built := baselineRecord(t, w)
		other := editBaselineRecord(t, w)
		require.NoError(t, openrails.Migrate(t.Context(), w.pool, w.cfg), "a schema equal to a fresh build is accepted whichever months it holds")
		require.Equal(t, built, baselineRecord(t, w), "the record is this build's again")
		var audited int
		require.NoError(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.migration_repairs
			WHERE app = 'openrails' AND database = 'postgres' AND schema = $1 AND sequence = 1 AND verb = 'verify-content' AND old_digest = $2 AND new_digest = $3`,
			w.schema, other[0], built[0]).Scan(&audited))
		require.Equal(t, 1, audited, "the re-stamp is on the record")
		var kept, remade bool
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL, to_regclass($2) IS NOT NULL",
			pgx.Identifier{w.schema, old}.Sanitize(), pgx.Identifier{w.schema, ahead}.Sanitize()).Scan(&kept, &remade))
		require.True(t, kept, "the migration step drops no month")
		require.True(t, remade, "and makes the months rows are written into now")

		reopened, err := openrails.New(t.Context(), w.cfg, w.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reopened.Close(context.Background())) })
		client, err := reopened.Merchant(w.alpha.MerchantID())
		require.NoError(t, err)
		balance(t, client, customer, 500, 100, 600)
		event, err := client.GetUsageEvent(t.Context(), *spent[0].Receipt.UsageEventID)
		require.NoError(t, err)
		require.EqualValues(t, 400, event.Amount)
		ledger, err := client.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 2)
		held, err := client.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("held", 100)})
		require.NoError(t, err)
		require.NoError(t, held[0].Err(), "the hold admitted before the migration step is still open")
		balance(t, client, customer, 500, 0, 500)
	})

	t.Run("OR-J31-S13/a-start-that-skips-migrations-is-refused-on-an-edited-baseline-until-the-migration-step-runs", func(t *testing.T) {
		w := newWorld(t, riverApart(t))
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "kept", DisplayName: "Kept"})
		require.NoError(t, err)
		require.NoError(t, w.engine.Close(t.Context()))
		built := baselineRecord(t, w)
		other := editBaselineRecord(t, w)

		skipping := w.cfg
		skipping.Database.SkipMigrations = true
		refused, err := openrails.New(t.Context(), skipping, w.deps)
		if err == nil {
			require.NoError(t, refused.Close(t.Context()))
		}
		require.ErrorIs(t, err, openrails.ErrMigrationsRequired, "a recorded name is not enough: the content is another baseline's")
		require.ErrorContains(t, err, "run openrails.Migrate before application startup")
		require.ErrorContains(t, err, "0001_schema.up.sql")
		require.NotContains(t, err.Error(), "\n")
		require.Equal(t, other, baselineRecord(t, w), "the refused start settles nothing")

		require.NoError(t, openrails.Migrate(t.Context(), w.pool, w.cfg))
		require.Equal(t, built, baselineRecord(t, w))
		started, err := openrails.New(t.Context(), skipping, w.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, started.Close(context.Background())) })
		client, err := started.Merchant(w.alpha.MerchantID())
		require.NoError(t, err)
		kept, err := client.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Equal(t, "Kept", kept.DisplayName)
	})
}
