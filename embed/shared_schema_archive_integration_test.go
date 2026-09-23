//go:build integration

package embed_test

import (
	"bytes"
	"context"
	"github.com/open-rails/openrails/config"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestSharedPublicArchiveLeavesHostTablesAndQueueUntouched(t *testing.T) {
	admin := migrationConcurrencyAdmin(t)
	id := merchant.ID(uuid.New())
	customer := uuid.New()
	makeBook := func() (*pgxpool.Pool, *db.DB) {
		pool := migrationConcurrencyPool(t, admin, 1)
		require.NoError(t, embed.ApplyMigrations(t.Context(), pool, embed.MigrationOptions{Schema: "public"}))
		bound, err := db.NewWithPGXPool(pool, "public")
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), `INSERT INTO public.merchants(id,slug) VALUES($1,'shared-book')`, id.UUID())
		require.NoError(t, err)
		// Host tables can have merchant_id too. Neither their name nor that column
		// gives OpenRails ownership or permission to export/delete their rows.
		_, err = pool.Exec(t.Context(), `CREATE TABLE public.host_records(merchant_id uuid PRIMARY KEY, value text)`)
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), `INSERT INTO public.host_records VALUES($1,'host-only')`, id.UUID())
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), `INSERT INTO public.river_job(kind,args,queue,state,scheduled_at) VALUES('host-pending','{}','host','scheduled',now()+interval '1 day')`)
		require.NoError(t, err)
		return pool, bound
	}
	sourcePool, source := makeBook()
	targetPool, target := makeBook()
	_, err := sourcePool.Exec(t.Context(), `INSERT INTO public.customers(merchant_id,id,issuer) VALUES($1,$2,'shared-issuer')`, id.UUID(), customer)
	require.NoError(t, err)
	before := func(pool *pgxpool.Pool) string {
		var s string
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT jsonb_build_array((SELECT jsonb_agg(h) FROM public.host_records h),(SELECT jsonb_agg(j) FROM public.river_job j),(SELECT jsonb_agg(m ORDER BY id) FROM public.migrations m))::text`).Scan(&s))
		return s
	}
	sourceBefore, targetBefore := before(sourcePool), before(targetPool)
	var archive bytes.Buffer
	require.NoError(t, merchantarchive.Export(t.Context(), source, id, &archive))
	require.NotContains(t, archive.String(), "host-only")
	require.NotContains(t, archive.String(), "host-pending")
	restored, err := merchantarchive.Restore(t.Context(), target, id, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	require.False(t, restored.Replayed)
	replay, err := merchantarchive.Restore(t.Context(), target, id, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, sourceBefore, before(sourcePool))
	require.Equal(t, targetBefore, before(targetPool))
	var count int
	require.NoError(t, targetPool.QueryRow(t.Context(), `SELECT count(*) FROM public.customers WHERE merchant_id=$1 AND id=$2`, id.UUID(), customer).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, source.Close())
	require.NoError(t, target.Close())
	require.NoError(t, sourcePool.Ping(context.Background()))
	require.NoError(t, targetPool.Ping(context.Background()))
}

func TestResetRefusesSharedDefaultSchemaBeforeMutation(t *testing.T) {
	admin := migrationConcurrencyAdmin(t)
	pool := migrationConcurrencyPool(t, admin, 1)
	require.NoError(t, embed.ApplyMigrations(t.Context(), pool, embed.MigrationOptions{Schema: "billing", River: embed.RiverManagedByOpenRails("billing")}))
	_, err := pool.Exec(t.Context(), `CREATE TABLE billing.host_records(id integer);INSERT INTO billing.host_records VALUES(7)`)
	require.NoError(t, err)
	dsnURL, err := url.Parse(pool.Config().ConnString())
	require.NoError(t, err)
	dsnURL.Path = "/" + pool.Config().ConnConfig.Database
	dsn := dsnURL.String()
	plan, err := migrate.PlanEmbeddedReset(t.Context(), dsn)
	require.NoError(t, err)
	_, err = migrate.ApplyEmbeddedReset(t.Context(), dsn, plan.Target, migrate.EmbeddedResetConfirmation(plan.Target))
	require.ErrorContains(t, err, "shared schema")
	var count int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.host_records WHERE id=7`).Scan(&count))
	require.Equal(t, 1, count)
	var intact bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT to_regclass('billing.river_job') IS NOT NULL AND to_regclass('billing.merchants') IS NOT NULL`).Scan(&intact))
	require.True(t, intact)
}

func TestSharedPublicManagedRuntimeExecutesBillingJobs(t *testing.T) {
	admin := migrationConcurrencyAdmin(t)
	pool := migrationConcurrencyPool(t, admin, 1)
	require.NoError(t, embed.ApplyMigrations(t.Context(), pool, embed.MigrationOptions{Schema: "public"}))
	dsnURL, err := url.Parse(pool.Config().ConnString())
	require.NoError(t, err)
	dsnURL.Path = "/" + pool.Config().ConnConfig.Database
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{Schema: "public", URL: dsnURL.String()}}
	runtime, err := embed.New(t.Context(), embed.Options{Config: cfg, PGXPool: pool, River: embed.RiverManagedByOpenRails("public")})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.RunWorkers(ctx) }()
	defer func() {
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		require.NoError(t, runtime.Close(context.Background()))
		require.NoError(t, pool.Ping(context.Background()))
	}()
	require.Eventually(t, func() bool {
		var n int
		err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.river_job WHERE kind LIKE 'openrails.%' AND state='completed'`).Scan(&n)
		return err == nil && n > 0
	}, 30*time.Second, 50*time.Millisecond)
	require.False(t, runtime.HasExternalRiverClient())
}
