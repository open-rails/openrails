//go:build integration

package embed_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/merchant"
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
	schema := compositionRiverSchema(t)
	dsn := dbtest.SharedPostgresDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	rdb, _ := dbtest.SharedRedisClient(t)
	var client *river.Client[pgx.Tx]
	var sawBillingWorkers bool

	slug := "river-billing-" + uuid.NewString()[:8]
	rt, err := embed.New(ctx, embed.Options{
		Merchant: &embed.MerchantDeclaration{Slug: slug, PSPs: []embed.PSPDeclaration{{Key: "solana", Rail: "solana", AccountID: "11111111111111111111111111111111"}}},
		Config: &config.Config{Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="},
			TestMode:           config.CredentialPostureSandbox,
			DB:                 &config.DBConfig{URL: dsn},
			MerchantConfigHTTP: true, SecretBackend: config.SecretBackendDB,
		},
		River: embed.RiverFromHost(),
		Redis: rdb,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	require.False(t, rt.HasExternalRiverClient())
	require.Nil(t, app.HostGraph(rt).Runtime.RiverProducer, "host producer is unavailable until composition")
	require.ErrorContains(t, rt.Ready(ctx), "not bound")
	_, err = rt.CheckJobProgress(ctx)
	require.ErrorContains(t, err, "not bound")
	require.ErrorContains(t, rt.RunWorkers(ctx), "not bound")
	cp, err := controlplane.Attach(ctx, rt, controlplane.Options{Auth: &hostconfig.AuthConfig{Issuer: "https://river-compose.test", KeysPath: t.TempDir(), AllowMemory: true, AllowEphemeralSigningKey: true, AllowMissingSenders: true, AllowPrivateNetworkJWKS: true, DirectPeerIP: true}})
	require.NoError(t, err)
	suffix := uuid.NewString()[:8]
	user, err := cp.Core().CreateUser(ctx, "river-"+suffix+"@example.test", "river"+suffix)
	require.NoError(t, err)
	billingClient, err := rt.Client()
	require.NoError(t, err)
	merchantID := billingClient.MerchantID()
	expired, alive := uuid.New(), uuid.New()
	for _, row := range []struct {
		id      uuid.UUID
		expires time.Time
	}{{expired, time.Now().Add(-time.Hour)}, {alive, time.Now().Add(time.Hour)}} {
		hash := sha256.Sum256([]byte(row.id.String()))
		_, err = pool.Exec(ctx, `INSERT INTO profiles.refresh_sessions(id,user_id,issuer,current_token_hash,expires_at) VALUES($1,$2::uuid,$3,$4,$5)`, row.id, user.ID, "https://river-compose.test", hash[:], row.expires)
		require.NoError(t, err)
	}
	client, err = riverhelpers.New(ctx, pool, &river.Config{Schema: schema, Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}, embed.QueueBilling: {MaxWorkers: 2}}}, rt.RiverJobs(), riverhelpers.NewContribution("host", func(_ context.Context, cfg *river.Config) error {
		require.NotNil(t, cfg.Workers)
		sawBillingWorkers = true
		return river.AddWorkerSafely(cfg.Workers, &noopWorker{})
	}, nil, nil))
	require.NoError(t, err)
	_, err = controlplane.Attach(ctx, rt, controlplane.Options{Auth: &hostconfig.AuthConfig{Issuer: "https://river-compose.test", KeysPath: t.TempDir(), AllowMemory: true, AllowEphemeralSigningKey: true, AllowMissingSenders: true, AllowPrivateNetworkJWKS: true, DirectPeerIP: true}})
	require.Error(t, err, "components cannot attach after binding")

	require.True(t, sawBillingWorkers, "explicit binding composes billing workers")
	require.True(t, rt.HasExternalRiverClient(), "explicit binding adopts the host client")
	require.NotNil(t, client)
	graph := app.HostGraph(rt).Runtime
	require.Same(t, client, graph.RiverProducer, "request producers use the host client")
	// Producers can atomically enqueue before workers start. A rolled-back
	// transaction leaves no job, and a committed job targets the host schema.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	rolledBack, err := graph.RiverProducer.InsertTx(ctx, tx, noopJobArgs{}, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE id=$1)", rolledBack.Job.ID).Scan(&exists))
	require.False(t, exists)
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	hostJob, err := graph.RiverProducer.InsertTx(ctx, tx, noopJobArgs{}, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() { _ = client.Stop(context.Background()) })
	// The real non-River poller must remove an expired pending reference. Its
	// RPC is pinned to loopback even though this expired entry needs no call.
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("expired reference unexpectedly called RPC: %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(rpc.Close)
	graph.SolanaRPCResolver.Endpoint = rpc.URL
	reference := solanago.NewWallet().PublicKey().String()
	mctx := merchant.WithID(ctx, merchantID)
	require.NoError(t, graph.SolanaPayService.RegisterPendingReference(mctx, reference))
	loopCtx, cancelLoops := context.WithCancel(ctx)
	loopDone := make(chan error, 1)
	go func() { loopDone <- rt.RunWorkers(loopCtx) }()
	var stopped sync.Once
	stopLoops := func() { stopped.Do(func() { cancelLoops(); require.ErrorIs(t, <-loopDone, context.Canceled) }) }
	t.Cleanup(stopLoops)
	require.Eventually(t, func() bool {
		pending, e := graph.SolanaPayService.PendingReferencesByMerchant(ctx)
		return e == nil && len(pending[merchantID]) == 0
	}, 30*time.Second, 100*time.Millisecond, "core non-River polling must run after explicit binding")
	require.Eventually(t, func() bool {
		var dead, live int
		e := pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE id=$1),count(*) FILTER(WHERE id=$2) FROM profiles.refresh_sessions WHERE id IN($1,$2)`, expired, alive).Scan(&dead, &live)
		return e == nil && dead == 0 && live == 1
	}, 30*time.Second, 100*time.Millisecond, "the same client's AuthKit schedule removes only expired sessions")
	require.Eventually(t, func() bool {
		var state string
		return pool.QueryRow(ctx, "SELECT state FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE id=$1", hostJob.Job.ID).Scan(&state) == nil && state == "completed"
	}, 30*time.Second, 100*time.Millisecond, "host jobs must run on the same client")

	// A billing job enqueued through the SHARED client is drained by the billing
	// worker the binder received — proving the registry was actually wired, not
	// just that the call didn't error.
	res, err := client.Insert(ctx, riverjobs.CleanupExpiredDataArgs{}, &river.InsertOpts{Queue: riverjobs.QueueBilling})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		var state string
		row := pool.QueryRow(ctx, "SELECT state FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE id=$1", res.Job.ID)
		return row.Scan(&state) == nil && state == "completed"
	}, 30*time.Second, 100*time.Millisecond, "billing job must complete on the host's shared client")

	// #895 state 2, structurally fixed: the host installed NO client-level
	// middleware, yet the worked job still recorded a success — the bookkeeping
	// rides on the worker itself, so a host cannot omit it.
	dbi := dbtest.OpenAppDB(t, dsn)
	var lastSuccess *time.Time
	require.NoError(t, dbi.Qx(ctx).QueryRow(ctx,
		`SELECT last_success_at FROM billing.worker_state WHERE worker_kind = $1`,
		riverjobs.CleanupExpiredDataArgs{}.Kind()).Scan(&lastSuccess))
	require.NotNil(t, lastSuccess, "health bookkeeping must be installed without host cooperation (#895)")

	// The fleet is progressing, and OpenRails can say so without running a job.
	report, err := rt.CheckJobProgress(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, report.Kinds, "periodic kinds are registered")
	require.NoError(t, report.Err())
	stopLoops()
	require.NoError(t, client.Stop(ctx))
	require.NoError(t, rt.Close(ctx))
	require.NoError(t, pool.Ping(ctx), "the host pool remains owned by the host")
}

// An omitted River option constructs the managed default fleet.
func TestRiverDefault_ConstructsManagedFleet(t *testing.T) {
	ctx := context.Background()
	rt, err := embed.New(ctx, embed.Options{Config: &config.Config{
		TestMode: config.CredentialPostureSandbox,
		DB:       &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)},
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(ctx)) })
	require.False(t, rt.HasExternalRiverClient())
}

// A host must supply the pool it owns before any fleet can be constructed.
func TestRiverFromHost_MissingPoolRefuses(t *testing.T) {
	ctx := t.Context()
	rt, err := embed.New(ctx, embed.Options{Config: &config.Config{TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}}, River: embed.RiverFromHost()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	_, err = riverhelpers.New(ctx, nil, nil, rt.RiverJobs())
	require.ErrorContains(t, err, "pool is required")
}
