//go:build integration

package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestControlPlaneClosesOwnedAuthKitPools(t *testing.T) {
	ctx := context.Background()
	observer := dbtest.SharedSuperuserPGXPool(t)
	cfg := observer.Config()
	name := "controlplane-owned-" + uuid.NewString()
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	host, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(host.Close)
	require.NoError(t, host.Ping(ctx))
	rdb, _ := dbtest.SharedRedisClient(t)
	cp, err := New(ctx, &config.Config{
		DB: &config.DBConfig{},
	}, &hostconfig.AuthConfig{Issuer: "https://ownership.test", MintDisabled: true, DirectPeerIP: true}, host, WithRedis(rdb))
	require.NoError(t, err)
	t.Cleanup(cp.Close)

	count := func() int {
		var n int
		require.NoError(t, observer.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, name).Scan(&n))
		return n
	}
	baseline := count()
	search := cp.MerchantGroupSearchResolver()
	for i := 0; i < 5; i++ {
		_, err := search(ctx, "", "", "", 10)
		require.NoError(t, err)
		require.Eventually(t, func() bool { return count() == baseline }, 5*time.Second, 10*time.Millisecond,
			"each search must close its temporary schema-bound directory pool")
	}
	cp.Close()
	cp.Close()
	require.NoError(t, host.Ping(ctx), "the caller still owns the source pool")
	require.Eventually(t, func() bool { return count() == int(host.Stat().TotalConns()) },
		5*time.Second, 10*time.Millisecond, "closing the control plane must leave only caller-owned connections")

	// Empty route selection must stay closed even though AuthKit's own mount
	// treats an empty selection as its default surface.
	groups := IntentionalRouteGroups
	IntentionalRouteGroups = nil
	t.Cleanup(func() { IntentionalRouteGroups = groups })
	closed, err := New(ctx, &config.Config{
		DB: &config.DBConfig{},
	}, &hostconfig.AuthConfig{Issuer: "https://ownership.test", MintDisabled: true, DirectPeerIP: true}, host, WithRedis(rdb))
	require.NoError(t, err)
	t.Cleanup(closed.Close)
	routes, err := closed.AuthRoutes()
	require.NoError(t, err)
	require.Empty(t, routes, "an empty OpenRails allow-list must not mount AuthKit defaults")
	closed.Close()
	require.NoError(t, host.Ping(ctx))
	require.Eventually(t, func() bool { return count() == int(host.Stat().TotalConns()) },
		5*time.Second, 10*time.Millisecond, "the zero-route HTTP surface is also runtime-owned")
}
