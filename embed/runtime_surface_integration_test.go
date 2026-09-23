//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
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

	declaration := embed.PSPDeclaration{Key: "legacy", Rail: "ccbill", AccountID: fmt.Sprintf("9%d", time.Now().UnixNano()%1e9)}
	rt, err := embed.New(ctx, embed.Options{
		Merchant: &embed.MerchantDeclaration{Slug: fmt.Sprintf("runtime-surface-%d", time.Now().UnixNano()), PSPs: []embed.PSPDeclaration{declaration, declaration}},
		Config:   &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: dsn}},
		PGXPool:  pool,
		River:    embed.RiverFromHost(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	_, err = riverhelpers.New(ctx, pool, &river.Config{Queues: map[string]river.QueueConfig{embed.QueueBilling: {MaxWorkers: 1}}}, rt.RiverJobs())
	require.NoError(t, err)
	require.True(t, rt.HasExternalRiverClient())
	require.NoError(t, rt.Ready(ctx))
	_, err = rt.CheckJobProgress(ctx)
	require.NoError(t, err)

	_, err = embed.New(ctx, embed.Options{Config: &config.Config{}, HTTP: &embed.HTTPConfig{CustomerRoutes: []embed.CustomerRoutesConfig{{Treasury: true}}}})
	require.ErrorContains(t, err, "requires its own authenticator", "the self surface never mounts without authentication")
}
