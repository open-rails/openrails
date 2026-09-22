//go:build integration

package embed_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/riverkit"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
)

// A private queue namespace prevents earlier scheduled jobs from suppressing
// this client's RunOnStart jobs through River's persisted uniqueness keys.
func compositionRiverSchema(t *testing.T) string {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dbtest.SharedSuperuserDSN(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	runtime, err := pgxpool.New(t.Context(), dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	defer runtime.Close()
	schema := "composition_" + uuid.NewString()[:8]
	require.NoError(t, embed.ApplyMigrations(t.Context(), pool, embed.MigrationOptions{River: embed.RiverManagedByOpenRails(schema), RuntimePool: runtime}))
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	return schema
}

func TestHostRiverCompositionRefusals(t *testing.T) {
	newRuntime := func(t *testing.T) (*embed.Runtime, *pgxpool.Pool) {
		t.Helper()
		dsn := dbtest.SharedPostgresDSN(t)
		pool, err := pgxpool.New(t.Context(), dsn)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		rt, err := embed.New(t.Context(), embed.Options{Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn}, Auth: &config.AuthConfig{Issuer: "https://compose.test", KeysPath: t.TempDir()}}, PGXPool: pool, River: embed.RiverFromHost()})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
		return rt, pool
	}
	t.Run("nil pool does not consume a retained descriptor", func(t *testing.T) {
		rt, pool := newRuntime(t)
		jobs := rt.RiverJobs()
		_, err := riverkit.New(t.Context(), nil, nil, jobs)
		require.ErrorContains(t, err, "pool is required")
		client, err := riverkit.New(t.Context(), pool, nil, jobs)
		require.NoError(t, err)
		require.Nil(t, client.Stopped())
	})
	t.Run("closed and duplicate runtimes", func(t *testing.T) {
		rt, pool := newRuntime(t)
		_, err := riverkit.New(t.Context(), pool, nil, rt.RiverJobs())
		require.NoError(t, err)
		_, err = riverkit.New(t.Context(), pool, nil, rt.RiverJobs())
		require.ErrorContains(t, err, "already sealed")
		require.True(t, rt.HasExternalRiverClient(), "refused new descriptor cannot abort the previously bound runtime")
		require.NoError(t, rt.Ready(t.Context()))
		require.NoError(t, rt.Close(t.Context()))
		_, err = riverkit.New(t.Context(), pool, nil, rt.RiverJobs())
		require.ErrorContains(t, err, "closed")
		require.NoError(t, pool.Ping(t.Context()))
	})
	t.Run("requesting jobs seals component attachment", func(t *testing.T) {
		rt, pool := newRuntime(t)
		jobs := rt.RiverJobs()
		_, err := controlplane.Attach(t.Context(), rt, controlplane.Options{})
		require.ErrorContains(t, err, "attach before")
		_, err = riverkit.New(t.Context(), pool, nil, jobs)
		require.NoError(t, err)
	})
	t.Run("attached AuthKit cannot be registered twice", func(t *testing.T) {
		rt, pool := newRuntime(t)
		cp, err := controlplane.Attach(t.Context(), rt, controlplane.Options{})
		require.NoError(t, err)
		_, err = riverkit.New(t.Context(), pool, nil, rt.RiverJobs(), cp.Core().RiverJobs())
		require.ErrorContains(t, err, "duplicate contribution")
		require.False(t, rt.HasExternalRiverClient())
	})
	t.Run("late binding failure invalidates partial producer", func(t *testing.T) {
		rt, pool := newRuntime(t)
		cause := errors.New("bind failure")
		bad := riverkit.NewContribution("bad", func(context.Context, *river.Config) error { return nil }, func(context.Context, *river.Client[pgx.Tx]) error { return cause }, func() error { return nil })
		client, err := riverkit.New(t.Context(), pool, nil, rt.RiverJobs(), bad)
		require.ErrorIs(t, err, cause)
		require.Nil(t, client)
		require.False(t, rt.HasExternalRiverClient())
		require.Error(t, rt.Ready(t.Context()))
		_, err = riverkit.New(t.Context(), pool, nil, rt.RiverJobs())
		require.ErrorContains(t, err, "sealed")
		require.NoError(t, pool.Ping(t.Context()))
	})
	for _, entry := range []struct {
		name   string
		mutate func(*river.Config)
		want   string
	}{
		{"registry", func(cfg *river.Config) { cfg.Workers = river.NewWorkers() }, "worker registry"},
		{"queue", func(cfg *river.Config) { delete(cfg.Queues, embed.QueueBilling) }, "queue"},
		{"periodics", func(cfg *river.Config) { cfg.PeriodicJobs = nil }, "periodic"},
		{"schema", func(cfg *river.Config) { cfg.Schema = "other" }, "schema"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			rt, pool := newRuntime(t)
			bad := riverkit.NewContribution("bad", func(_ context.Context, cfg *river.Config) error { entry.mutate(cfg); return nil }, nil, nil)
			client, err := riverkit.New(t.Context(), pool, nil, rt.RiverJobs(), bad)
			require.ErrorContains(t, err, entry.want)
			require.Nil(t, client)
			require.False(t, rt.HasExternalRiverClient())
		})
	}
}

func TestManagedRiverStillComposesControlPlaneBeforeRunWorkers(t *testing.T) {
	ctx := t.Context()
	schema := compositionRiverSchema(t)
	dsn := dbtest.SharedPostgresDSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	rt, err := embed.New(ctx, embed.Options{Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn}, Auth: &config.AuthConfig{Issuer: "https://managed-compose.test", KeysPath: t.TempDir()}}, River: embed.RiverManagedByOpenRails(schema)})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
	cp, err := controlplane.Attach(ctx, rt, controlplane.Options{})
	require.NoError(t, err)
	suffix := uuid.NewString()[:8]
	user, err := cp.Core().CreateUser(ctx, "managed-"+suffix+"@example.test", "managed"+suffix)
	require.NoError(t, err)
	session := uuid.New()
	hash := sha256.Sum256([]byte(session.String()))
	_, err = pool.Exec(ctx, `INSERT INTO profiles.refresh_sessions(id,user_id,issuer,current_token_hash,expires_at) VALUES($1,$2::uuid,'https://managed-compose.test',$3,now()-interval '1 hour')`, session, user.ID, hash[:])
	require.NoError(t, err)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- rt.RunWorkers(runCtx) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); require.ErrorIs(t, <-done, context.Canceled) }) }
	t.Cleanup(stop)
	require.Eventually(t, func() bool {
		var n int
		return pool.QueryRow(ctx, `SELECT count(*) FROM profiles.refresh_sessions WHERE id=$1`, session).Scan(&n) == nil && n == 0
	}, 30*time.Second, 100*time.Millisecond, "managed RunWorkers must include attached AuthKit maintenance")
	require.NoError(t, rt.Ready(ctx))
	require.False(t, rt.HasExternalRiverClient())
	stop()
	require.NoError(t, rt.Close(ctx))
}
