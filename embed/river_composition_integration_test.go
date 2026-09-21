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
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
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
	schema := "composition_" + uuid.NewString()[:8]
	require.NoError(t, embed.ApplyMigrations(t.Context(), pool, embed.MigrationOptions{River: embed.RiverManagedByOpenRails(schema)}))
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
		rt, err := embed.New(t.Context(), embed.Options{Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn}}, River: embed.RiverFromHost()})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
		return rt, pool
	}
	t.Run("nil binder and closed runtime", func(t *testing.T) {
		rt, _ := newRuntime(t)
		_, err := rt.BindRiver(t.Context(), nil)
		require.ErrorContains(t, err, "binder is required")
		require.NoError(t, rt.Close(t.Context()))
		called := false
		_, err = rt.BindRiver(t.Context(), func(context.Context, *river.Config) (*river.Client[pgx.Tx], error) { called = true; return nil, nil })
		require.ErrorContains(t, err, "closed")
		require.False(t, called)
		require.Error(t, rt.Ready(t.Context()))
		require.ErrorContains(t, rt.RunWorkers(t.Context()), "closed")
	})
	t.Run("double binding never repeats construction", func(t *testing.T) {
		rt, pool := newRuntime(t)
		calls := 0
		bind := func(_ context.Context, cfg *river.Config) (*river.Client[pgx.Tx], error) {
			calls++
			return river.NewClient(riverpgxv5.New(pool), cfg)
		}
		_, err := rt.BindRiver(t.Context(), bind)
		require.NoError(t, err)
		_, err = rt.BindRiver(t.Context(), bind)
		require.ErrorContains(t, err, "already bound")
		require.Equal(t, 1, calls)
	})
	t.Run("concurrent binding claims one composition", func(t *testing.T) {
		rt, pool := newRuntime(t)
		entered, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := rt.BindRiver(t.Context(), func(_ context.Context, cfg *river.Config) (*river.Client[pgx.Tx], error) {
				close(entered)
				<-release
				return river.NewClient(riverpgxv5.New(pool), cfg)
			})
			done <- err
		}()
		<-entered
		secondCalled := false
		_, err := rt.BindRiver(t.Context(), func(context.Context, *river.Config) (*river.Client[pgx.Tx], error) {
			secondCalled = true
			return nil, nil
		})
		close(release)
		require.ErrorContains(t, err, "already bound")
		require.False(t, secondCalled)
		require.NoError(t, <-done)
	})
	t.Run("failed registration is fatal setup", func(t *testing.T) {
		rt, _ := newRuntime(t)
		cause := errors.New("host construction refused")
		_, err := rt.BindRiver(t.Context(), func(context.Context, *river.Config) (*river.Client[pgx.Tx], error) { return nil, cause })
		require.ErrorIs(t, err, cause)
		require.ErrorContains(t, rt.Ready(t.Context()), "not bound")
		_, err = rt.BindRiver(t.Context(), func(context.Context, *river.Config) (*river.Client[pgx.Tx], error) {
			t.Fatal("partial registration must not retry")
			return nil, nil
		})
		require.ErrorContains(t, err, "already bound")
	})
	for _, entry := range []struct {
		name   string
		mutate func(*river.Config)
		want   string
	}{
		{"worker registry", func(cfg *river.Config) { cfg.Workers = river.NewWorkers(); river.AddWorker(cfg.Workers, &noopWorker{}) }, "worker registry"},
		{"required queue", func(cfg *river.Config) {
			delete(cfg.Queues, embed.QueueBilling)
			cfg.Queues[river.QueueDefault] = river.QueueConfig{MaxWorkers: 1}
		}, "required River queue"},
		{"periodic jobs", func(cfg *river.Config) { cfg.PeriodicJobs = nil }, "periodic jobs"},
		{"billing namespace", func(cfg *river.Config) { cfg.Schema = "billing" }, "billing schema"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			rt, pool := newRuntime(t)
			returned, err := rt.BindRiver(t.Context(), func(_ context.Context, cfg *river.Config) (*river.Client[pgx.Tx], error) {
				entry.mutate(cfg)
				return river.NewClient(riverpgxv5.New(pool), cfg)
			})
			require.ErrorContains(t, err, entry.want)
			require.NotNil(t, returned, "a host-created client stays visible for caller cleanup")
			require.Nil(t, returned.Stopped(), "the runtime never starts the rejected client")
			require.False(t, rt.HasExternalRiverClient())
		})
	}
	t.Run("started client stays owned by host", func(t *testing.T) {
		rt, pool := newRuntime(t)
		client, err := rt.BindRiver(t.Context(), func(ctx context.Context, cfg *river.Config) (*river.Client[pgx.Tx], error) {
			client, err := river.NewClient(riverpgxv5.New(pool), cfg)
			if err != nil {
				return nil, err
			}
			return client, client.Start(ctx)
		})
		require.ErrorContains(t, err, "unstarted")
		require.NotNil(t, client)
		t.Cleanup(func() { require.NoError(t, client.Stop(context.Background())) })
		select {
		case <-client.Stopped():
			t.Fatal("binding stopped a client the host started")
		default:
		}
		require.False(t, rt.HasExternalRiverClient())
		require.NoError(t, client.Stop(t.Context()))
		require.NoError(t, rt.Close(t.Context()))
	})
}

func TestManagedRiverStillComposesControlPlaneBeforeRunWorkers(t *testing.T) {
	ctx := t.Context()
	schema := compositionRiverSchema(t)
	dsn := dbtest.SharedPostgresDSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	rt, err := embed.New(ctx, embed.Options{Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn}, Auth: &config.AuthConfig{Issuer: "https://managed-compose.test", KeysPath: t.TempDir()}}, River: embed.RiverManagedByOpenRails(schema)})
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
