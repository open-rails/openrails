//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
)

// A host with its own River client uses the Runtime for readiness, fleet
// progress and PSP declaration without reaching the engine behind it.
func TestRuntimeOwnsReadinessAndRiverChecks(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var jobs *river.Client[pgx.Tx]
	rt, err := embed.New(ctx, embed.Options{
		Config:  &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: dsn}},
		PGXPool: pool,
		River: embed.RiverFromHost(func(_ context.Context, fleet *embed.RiverFleet) (*river.Client[pgx.Tx], error) {
			jobs, err = river.NewClient(riverpgxv5.New(pool), &river.Config{
				Workers: fleet.Workers, Schema: fleet.Schema,
				Queues: map[string]river.QueueConfig{fleet.QueueBilling: {MaxWorkers: 1}},
			})
			return jobs, err
		}),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	require.True(t, rt.HasExternalRiverClient())
	require.NoError(t, rt.Ready(ctx))
	_, err = rt.CheckJobProgress(ctx)
	require.NoError(t, err)

	merchantID, err := rt.UpsertMerchantConfig(ctx, fmt.Sprintf("runtime-surface-%d", time.Now().UnixNano()), embed.MerchantConfig{})
	require.NoError(t, err)
	declaration := embed.PSPDeclaration{Key: "legacy", Rail: "ccbill", AccountID: fmt.Sprintf("9%d", time.Now().UnixNano()%1e9)}
	first, err := rt.DeclarePSP(ctx, merchantID, declaration)
	require.NoError(t, err)
	again, err := rt.DeclarePSP(ctx, merchantID, declaration)
	require.NoError(t, err)
	require.Equal(t, first, again, "declaration is idempotent")

	_, err = rt.SelfHandler(nil)
	require.ErrorContains(t, err, "DelegatedAuthenticator", "the self surface never mounts without authentication")
}
