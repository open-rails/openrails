//go:build integration

package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestStandaloneWorkerIncludesAuthKitLifecycle(t *testing.T) {
	cfg := &config.Config{
		Env: "dev", APIURL: "http://127.0.0.1:3053",
		DB:                   &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)},
		Redis:                &config.RedisConfig{Addr: dbtest.SharedRedisAddr(t)},
		MerchantConfigSource: config.MerchantConfigSourceAPI,
		SecretBackend:        config.SecretBackendDB,
	}
	application, err := serverboot.NewWorker(t.Context(), cfg, &serverboot.Options{Auth: &hostconfig.AuthConfig{Issuer: "https://worker-recovery.test", KeysPath: t.TempDir(), DirectPeerIP: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, application.Close(context.Background())) })
	require.NoError(t, application.Runtime.InitRiver(t.Context()))
	cp := embcp.Get(application)
	require.NotNil(t, cp)
	name := "worker-recovery-" + uuid.NewString()[:8]
	user, err := cp.Core().CreateUser(t.Context(), name+"@example.test", name)
	require.NoError(t, err)
	deleted, err := cp.Core().SoftDeleteUsers(t.Context(), []string{user.ID})
	require.NoError(t, err)
	require.NoError(t, deleted[0].Err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- application.Runtime.RunWorkers(ctx) }()
	t.Cleanup(func() {
		cancel()
		err := <-done
		require.True(t, err == nil || errors.Is(err, context.Canceled), "%v", err)
	})
	pool := dbtest.SharedSuperuserPGXPool(t)
	require.Eventually(t, func() bool {
		var completed bool
		err := pool.QueryRow(t.Context(), "SELECT completed_at IS NOT NULL FROM profiles.account_deletion_deliveries WHERE user_id=$1::uuid AND stage='soft'", user.ID).Scan(&completed)
		return err == nil && completed
	}, 10*time.Second, 25*time.Millisecond, "the worker-only command must drain control-plane lifecycle jobs")
}

func TestStandaloneWorkerLoadsHostCredentialManifest(t *testing.T) {
	var hits atomic.Int64
	probe := fakeNMIProbeServer(t, &hits)
	t.Cleanup(probe.Close)
	dir := t.TempDir()
	manifest := writeMode1Manifest(t, dir)
	configPath := writeMode1Config(t, dir, dbtest.SharedPostgresDSN(t), freeTCPPort(t), "manifest", testSigningKeyPEM(t))
	loaded, err := hostconfig.Load(configPath)
	require.NoError(t, err)
	cfg := loaded.Config
	application, err := serverboot.NewWorker(t.Context(), cfg, &serverboot.Options{Auth: loaded.Auth, MerchantManifestPath: manifest, NMIProbeV5BaseURL: probe.URL})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, application.Close(context.Background())) })
	var id string
	require.NoError(t, dbtest.SharedSuperuserPGXPool(t).QueryRow(t.Context(), "SELECT id::text FROM billing.merchants WHERE slug=$1", mode1TestMerchantSlug).Scan(&id))
	key, err := merchants.PSPSecretName("nmi", "test", "100001", "security_key")
	require.NoError(t, err)
	secret, err := application.Runtime.ManifestSecrets.Get(t.Context(), merchant.ID(uuid.MustParse(id)), key)
	require.NoError(t, err)
	require.Equal(t, "sandbox-security-key", secret.Value)
	require.Positive(t, hits.Load(), "worker provisioning must validate the configured provider through the loopback probe")
	var persisted int
	require.NoError(t, dbtest.SharedSuperuserPGXPool(t).QueryRow(t.Context(), "SELECT count(*) FROM billing.merchant_secrets WHERE merchant_id=$1::uuid", id).Scan(&persisted))
	require.Zero(t, persisted, "host credentials stay in memory, not a new secret store")
	_, err = serverboot.NewWorker(t.Context(), cfg, &serverboot.Options{Auth: loaded.Auth, MerchantManifestPath: filepath.Join(dir, "missing.yaml")})
	require.ErrorContains(t, err, "missing.yaml", "an explicit missing manifest cannot silently start an unarmed worker")
	root := newRootCmd()
	worker, _, err := root.Find([]string{"run-worker"})
	require.NoError(t, err)
	require.NotNil(t, worker.Flags().Lookup("merchant-manifest"))
}
