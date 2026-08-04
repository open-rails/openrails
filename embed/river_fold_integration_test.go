//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

// noopWorker is a trivial host worker added to the SAME registry as billing's
// workers, proving the #895 binder contract: River fixes Workers at NewClient
// time, so the binder is the one moment host and billing workers can be merged.
type noopJobArgs struct{}

func (noopJobArgs) Kind() string { return "embed_test_noop" }

type noopWorker struct {
	river.WorkerDefaults[noopJobArgs]
}

func (noopWorker) Work(context.Context, *river.Job[noopJobArgs]) error { return nil }

// TestRiverFromHost_SharedClientDrainsBillingJobs proves state (1) of #895 —
// correct wiring — end to end on the real path: a host declares ownership via
// Options.River, builds the shared client inside the binder, and the resulting
// client (a) is the one the engine reports as external, (b) actually drains a
// billing job, and (c) carries billing's periodic jobs, which OpenRails
// registered itself rather than trusting the host to.
func TestRiverFromHost_SharedClientDrainsBillingJobs(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var client *river.Client[pgx.Tx]
	var sawBillingWorkers bool

	rt, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{
			Config: &config.Config{
				Env:      "dev",
				TestMode: config.CredentialPostureSandbox,
				DB:       &config.DBConfig{URL: dsn},
			},
			River: embedded.RiverFromHost(func(ctx context.Context, fleet *embedded.RiverFleet) (*river.Client[pgx.Tx], error) {
				// Billing's workers are ALREADY in the registry, with their
				// health bookkeeping attached — the host adds its own on top.
				require.NotNil(t, fleet.Workers)
				require.Equal(t, riverjobs.QueueBilling, fleet.QueueBilling)
				sawBillingWorkers = true
				require.NoError(t, river.AddWorkerSafely(fleet.Workers, &noopWorker{}))

				c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
					Queues: map[string]river.QueueConfig{
						river.QueueDefault:     {MaxWorkers: 2},
						fleet.QueueBilling:     {MaxWorkers: 2},
						riverjobs.QueueBilling: {MaxWorkers: 2},
					},
					Workers: fleet.Workers,
				})
				if err != nil {
					return nil, err
				}
				client = c
				return c, nil
			}),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	require.True(t, sawBillingWorkers, "binder must be invoked during New")
	emb := rt.Embedded()
	require.True(t, emb.HasExternalRiverClient(), "the binder's client must be injected by New")
	require.NotNil(t, client)

	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() { _ = client.Stop(context.Background()) })

	// A billing job enqueued through the SHARED client is drained by the billing
	// worker the binder received — proving the registry was actually wired, not
	// just that the call didn't error.
	res, err := client.Insert(ctx, riverjobs.CleanupExpiredDataArgs{}, &river.InsertOpts{Queue: riverjobs.QueueBilling})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		var state string
		row := pool.QueryRow(ctx, `SELECT state FROM river_job WHERE id = $1`, res.Job.ID)
		return row.Scan(&state) == nil && state == "completed"
	}, 30*time.Second, 100*time.Millisecond, "billing job must complete on the host's shared client")

	// #895 state 2, structurally fixed: the host installed NO client-level
	// middleware, yet the worked job still recorded a success — the bookkeeping
	// rides on the worker itself, so a host cannot omit it.
	dbi := dbtest.OpenAppDB(t, dsn)
	var lastSuccess *time.Time
	require.NoError(t, dbi.Qx(ctx).QueryRow(ctx,
		`SELECT last_success_at FROM openrails.worker_health WHERE worker_kind = $1`,
		riverjobs.CleanupExpiredDataArgs{}.Kind()).Scan(&lastSuccess))
	require.NotNil(t, lastSuccess, "health bookkeeping must be installed without host cooperation (#895)")

	// The fleet is progressing, and OpenRails can say so without running a job.
	report, err := emb.CheckJobProgress(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, report.Kinds, "periodic kinds are registered")
	require.NoError(t, report.Err())
}

// TestRiverRequired_ConstructionRefuses is the #895 headline: an embedded host
// that never declares River ownership must NOT get a usable engine. Before this
// change the same Options silently produced a working-looking engine whose
// money-moving periodic jobs would never run.
func TestRiverRequired_ConstructionRefuses(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	_, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{
			Config: &config.Config{
				Env:      "dev",
				TestMode: config.CredentialPostureSandbox,
				DB:       &config.DBConfig{URL: dsn},
			},
			// River deliberately omitted.
		},
	})
	require.ErrorIs(t, err, embedded.ErrRiverRequired)
}

// TestRiverFromHost_NilClientRefuses closes the other half of the handoff: a
// host may not declare ownership and then hand back nothing.
func TestRiverFromHost_NilClientRefuses(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	_, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{
			Config: &config.Config{
				Env:      "dev",
				TestMode: config.CredentialPostureSandbox,
				DB:       &config.DBConfig{URL: dsn},
			},
			River: embedded.RiverFromHost(func(context.Context, *embedded.RiverFleet) (*river.Client[pgx.Tx], error) {
				return nil, nil
			}),
		},
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "nil client")
}
