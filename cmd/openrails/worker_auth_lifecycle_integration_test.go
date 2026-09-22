//go:build integration

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/stretchr/testify/require"
)

func TestStandaloneWorkerIncludesAuthKitLifecycle(t *testing.T) {
	cfg := &config.Config{
		Env: "dev", APIURL: "http://127.0.0.1:3053",
		DB:                   &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)},
		Redis:                &config.RedisConfig{Addr: dbtest.SharedRedisAddr(t)},
		Auth:                 &config.AuthConfig{Issuer: "https://worker-recovery.test", KeysPath: t.TempDir(), DirectPeerIP: true},
		MerchantConfigSource: config.MerchantConfigSourceAPI,
		SecretBackend:        config.SecretBackendDB,
	}
	application, err := serverboot.NewWorker(t.Context(), cfg)
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
