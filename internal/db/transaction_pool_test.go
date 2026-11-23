//go:build integration

package db_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransactionPooler(t *testing.T) {
	queryURL, directURL := os.Getenv("OPENRAILS_TEST_PGBOUNCER_DSN"), os.Getenv("OPENRAILS_TEST_DIRECT_POSTGRES_DSN")
	if queryURL == "" || directURL == "" {
		t.Skip("set both disposable PostgreSQL and PgBouncer fixture URLs")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	query, err := pgxpool.ParseConfig(queryURL)
	require.NoError(t, err)
	query.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	query.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, query)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	direct, err := pgxpool.ParseConfig(directURL)
	require.NoError(t, err)
	direct.MaxConns = 6
	baselinePool, err := pgxpool.NewWithConfig(ctx, direct.Copy())
	require.NoError(t, err)
	t.Cleanup(baselinePool.Close)
	baseline, err := db.NewWithPGXPool(baselinePool, "billing")
	require.NoError(t, err)
	direct.ConnConfig.RuntimeParams["application_name"] = "openrails-pooler-test-session"
	database, err := db.NewWithPGXPoolAndSessions(pool, "billing", direct)
	require.NoError(t, err)

	ids := []billing.MerchantID{billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())}
	workload := func(t *testing.T, database *db.DB) int {
		t.Helper()
		type result struct {
			want, got string
			pid       int32
			err       error
		}
		results, done := make(chan result, 6), make(chan struct{})
		defer close(done)
		for i := range 6 {
			go func(mid billing.MerchantID) {
				out := result{want: mid.String()}
				scope, release, err := database.WithMerchantConn(merchant.WithID(ctx, mid))
				defer release()
				if err == nil {
					err = database.Qx(scope).QueryRow(scope, `SELECT current_setting('openrails.merchant_id', true), pg_backend_pid()`).Scan(&out.got, &out.pid)
				}
				out.err = err
				results <- out
				<-done
			}(ids[i%len(ids)])
		}
		backends := map[int32]bool{}
		for range 6 {
			select {
			case out := <-results:
				require.NoError(t, out.err)
				require.Equal(t, out.want, out.got)
				backends[out.pid] = true
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		return len(backends)
	}
	t.Run("merchant-scope-and-server-connections", func(t *testing.T) {
		before, after := workload(t, baseline), workload(t, database)
		t.Logf("six concurrent request scopes used %d direct backends and %d through PgBouncer", before, after)
		require.Equal(t, 6, before)
		require.Less(t, after, before)
		var clean bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(current_setting('openrails.merchant_id', true), '') = ''`).Scan(&clean))
		require.True(t, clean, "the merchant setting must expire at commit")
		for _, mid := range ids {
			scoped := merchant.WithID(ctx, mid)
			require.NoError(t, database.RunInMerchantConn(scoped, func(scoped context.Context) error {
				return database.ReadSnapshot(scoped, func(txCtx context.Context, tx pgx.Tx) error {
					var id, isolation string
					if err := tx.QueryRow(txCtx, `SELECT current_setting('openrails.merchant_id'), current_setting('transaction_isolation')`).Scan(&id, &isolation); err != nil {
						return err
					}
					require.Equal(t, mid.String(), id)
					require.Equal(t, "repeatable read", isolation)
					return nil
				})
			}))
		}
	})

	t.Run("listener-reconnects-on-the-direct-endpoint", func(t *testing.T) {
		listenCtx, stop := context.WithCancel(ctx)
		defer stop()
		verifier := &reconcile.Verifier{DB: database}
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			verifier.Listen(listenCtx)
		}()
		listenPID := func(prior int32) int32 {
			var pid int32
			require.Eventually(t, func() bool {
				return baselinePool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE application_name = 'openrails-pooler-test-session' AND query LIKE 'LISTEN %' AND pid <> $1`, prior).Scan(&pid) == nil
			}, 8*time.Second, 50*time.Millisecond)
			return pid
		}
		first := listenPID(0)
		var terminated bool
		require.NoError(t, baselinePool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, first).Scan(&terminated))
		require.True(t, terminated)
		require.NotEqual(t, first, listenPID(first))
		stop()
		select {
		case <-finished:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	})

	t.Run("provider-lock-stays-with-the-direct-session", func(t *testing.T) {
		id := ids[0].UUID()
		release, held, err := reconcile.LockMerchantPull(ctx, database, id)
		require.NoError(t, err)
		require.True(t, held)
		t.Cleanup(release)
		_, held, err = reconcile.LockMerchantPull(ctx, database, id)
		require.NoError(t, err)
		require.False(t, held)
		release()
		again, held, err := reconcile.LockMerchantPull(ctx, database, id)
		require.NoError(t, err)
		require.True(t, held)
		again()
	})

	t.Run("embedded-engine-migrates-and-runs-river", func(t *testing.T) {
		schema := "pooler_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := baselinePool.Exec(cleanup, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			require.NoError(t, err)
		}()
		cfg := openrails.Config{
			Database:          openrails.DatabaseConfig{Schema: schema, RiverSchema: schema},
			ProviderWriteMode: openrails.ProviderWritesReadOnly,
			Merchants: []openrails.MerchantDeclaration{
				{MerchantDocument: openrails.MerchantDocument{Slug: "alpha", DisplayName: "Alpha"}},
				{MerchantDocument: openrails.MerchantDocument{Slug: "beta", DisplayName: "Beta"}},
			},
		}
		bill, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool, PostgresSession: direct})
		require.NoError(t, err)
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, bill.Close(cleanup))
		}()
		alpha, err := bill.DeclaredMerchant("alpha")
		require.NoError(t, err)
		beta, err := bill.DeclaredMerchant("beta")
		require.NoError(t, err)
		require.NotEqual(t, alpha, beta)
		require.NoError(t, bill.Start(ctx))
		require.NoError(t, bill.Ready(ctx))
		table := pgx.Identifier{schema, "river_job"}.Sanitize()
		for _, kind := range []string{riverjobs.KindCleanupExpiredData, riverjobs.KindBillingPass} {
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				var state, jobErrors string
				err := baselinePool.QueryRow(ctx, "SELECT state, coalesce(errors::text, '') FROM "+table+" WHERE kind = $1 ORDER BY id LIMIT 1", kind).Scan(&state, &jobErrors)
				assert.NoError(c, err, kind)
				assert.Equal(c, "completed", state, "%s errors: %s", kind, jobErrors)
			}, 15*time.Second, 200*time.Millisecond)
		}
	})
}
