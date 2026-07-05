//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/embedded"
)

// noopWorker is a trivial host worker folded into the SAME registry as
// billing's workers, proving FoldIntoRiver's contract: workers must be fixed
// BEFORE river.NewClient, so both the host's own worker and billing's workers
// must already be in the same *river.Workers by the time the client is built.
type noopJobArgs struct{}

func (noopJobArgs) Kind() string { return "embed_test_noop" }

type noopWorker struct {
	river.WorkerDefaults[noopJobArgs]
}

func (noopWorker) Work(context.Context, *river.Job[noopJobArgs]) error { return nil }

// TestFoldIntoRiver_HostSharedClientDrainsBillingJobs proves the #546 recipe
// end-to-end: fold billing's workers/periodic jobs into a host-owned
// river.Workers/river.Client (the exact shape doujins/hentai0 hand-wrote as
// Service.RegisterRiverWorkers/RegisterRiverClient), then confirm the
// resulting SHARED client (a) is the one the embedded engine now reports via
// HasExternalRiverClient, (b) actually drains a billing job enqueued through
// it, and (c) surfaces billing's periodic jobs for the host to register.
func TestFoldIntoRiver_HostSharedClientDrainsBillingJobs(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	rt, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{
			Config: &config.Config{
				Env:      "dev",
				TestMode: config.CredentialPostureSandbox,
				DB:       &config.DBConfig{URL: dsn},
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	emb := rt.Embedded()
	require.False(t, emb.HasExternalRiverClient(), "no client injected yet")

	// Host builds its own worker registry with its own worker already added —
	// FoldIntoRiver must be callable on top of that, not require an empty one.
	workers := river.NewWorkers()
	river.AddWorkerSafely(workers, &noopWorker{})

	periodic, register, err := embedded.FoldIntoRiver(ctx, emb, workers)
	require.NoError(t, err)
	require.NotEmpty(t, periodic, "billing has at least one periodic job to fold in")

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault:     {MaxWorkers: 2},
			riverjobs.QueueBilling: {MaxWorkers: 2},
		},
		Workers: workers,
	})
	require.NoError(t, err)

	require.NoError(t, register(client))
	require.True(t, emb.HasExternalRiverClient(), "register must inject the client (SetRiverClient)")

	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() { _ = client.Stop(context.Background()) })

	// A billing job enqueued through the SHARED client is drained by the
	// billing worker FoldIntoRiver added to `workers` — proving AddWorkersTo
	// actually wired the worker, not just that the call didn't error.
	res, err := client.Insert(ctx, riverjobs.CleanupExpiredDataArgs{}, &river.InsertOpts{Queue: riverjobs.QueueBilling})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		var state string
		row := pool.QueryRow(ctx, `SELECT state FROM river_job WHERE id = $1`, res.Job.ID)
		return row.Scan(&state) == nil && state == "completed"
	}, 5*time.Second, 100*time.Millisecond, "billing job must complete on the host's shared client")
}

// TestFoldIntoRiver_NilEmbeddedErrors pins the not-initialized guard: calling
// FoldIntoRiver before the engine exists must error, not panic.
func TestFoldIntoRiver_NilEmbeddedErrors(t *testing.T) {
	_, _, err := embedded.FoldIntoRiver(context.Background(), nil, river.NewWorkers())
	require.ErrorIs(t, err, embedded.ErrNotInitialized)
}
