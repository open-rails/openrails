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
		spend(t, w.alpha, customer, "spent", 400)
		balance(t, w.alpha, customer, 600, 400, 1000)
		require.NoError(t, w.engine.Close(t.Context()))

		built := baselineRecord(t, w)
		other := editBaselineRecord(t, w)
		require.NoError(t, openrails.Migrate(t.Context(), w.pool, w.cfg.Database), "a schema equal to a fresh build is accepted")
		require.Equal(t, built, baselineRecord(t, w), "the record is this build's again")
		var audited int
		require.NoError(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.migration_repairs
			WHERE app = 'openrails' AND database = 'postgres' AND schema = $1 AND sequence = 1 AND verb = 'verify-content' AND old_digest = $2 AND new_digest = $3`,
			w.schema, other[0], built[0]).Scan(&audited))
		require.Equal(t, 1, audited, "the re-stamp is on the record")

		reopened, err := openrails.New(t.Context(), w.cfg, w.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reopened.Close(context.Background())) })
		client, err := reopened.Merchant(w.alpha.MerchantID())
		require.NoError(t, err)
		balance(t, client, customer, 600, 400, 1000)
		ledger, err := client.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 1, "the usage is held until its day posts")
		spend(t, client, customer, "after", 100)
		balance(t, client, customer, 500, 500, 1000)
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
		require.ErrorIs(t, err, billing.ErrMigrationsRequired, "a recorded name is not enough: the content is another baseline's")
		require.ErrorContains(t, err, "run openrails.Migrate before application startup")
		require.ErrorContains(t, err, "0001_schema.up.sql")
		require.NotContains(t, err.Error(), "\n")
		require.Equal(t, other, baselineRecord(t, w), "the refused start settles nothing")

		require.NoError(t, openrails.Migrate(t.Context(), w.pool, w.cfg.Database))
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
