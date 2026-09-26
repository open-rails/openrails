package app

import (
	"context"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/open-rails/openrails/config"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// periodicRunOnStart reads River's unexported opts; River exposes no accessor.
func periodicRunOnStart(job *river.PeriodicJob) bool {
	v := reflect.ValueOf(job).Elem().FieldByName("opts")
	opts := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(*river.PeriodicJobOpts)
	return opts != nil && opts.RunOnStart
}

func periodicArgs(job *river.PeriodicJob) (river.JobArgs, *river.InsertOpts) {
	v := reflect.ValueOf(job).Elem().FieldByName("constructorFunc")
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(river.PeriodicJobConstructor)()
}

// The schedule is the product's recovery contract: which sweeps run the moment
// a process restarts, and the cadence the staleness alert expects of each kind.
func TestPeriodicScheduleContract(t *testing.T) {
	want := map[string]struct {
		period     time.Duration
		runOnStart bool
	}{
		riverjobs.DunningArgs{}.Kind():                   {riverjobs.DuePassInterval, true},
		riverjobs.ProviderRefreshArgs{}.Kind():           {4 * time.Hour, true},
		riverjobs.JobRescueArgs{}.Kind():                 {time.Minute, true},
		riverjobs.PlanMigrationRedriveArgs{}.Kind():      {time.Hour, true},
		riverjobs.AccountUpdaterBatchArgs{}.Kind():       {6 * time.Hour, true},
		riverjobs.MerchantSecretCleanupArgs{}.Kind():     {5 * time.Minute, true},
		riverjobs.ConvergeSweepArgs{}.Kind():             {15 * time.Minute, true},
		riverjobs.LedgerIntegrityArgs{}.Kind():           {24 * time.Hour, true},
		riverjobs.CleanupExpiredDataArgs{}.Kind():        {time.Hour, false},
		riverjobs.IdempotencyGCArgs{}.Kind():             {15 * time.Minute, false},
		riverjobs.SolanaCrankArgs{}.Kind():               {time.Hour, false},
		riverjobs.SolanaGasAlertArgs{}.Kind():            {6 * time.Hour, false},
		riverjobs.SolanaReconcileArgs{}.Kind():           {6 * time.Hour, false},
		riverjobs.SolanaPayGCArgs{}.Kind():               {15 * time.Minute, false},
		riverjobs.CreditExpiryArgs{}.Kind():              {time.Hour, false},
		riverjobs.AdmissionDenialFlushArgs{}.Kind():      {5 * time.Minute, false},
		riverjobs.CatalogReconciliationPullArgs{}.Kind(): {time.Hour, false},
		riverjobs.StripeWebhookReconcileArgs{}.Kind():    {time.Hour, false},
		riverjobs.InvoiceArgs{}.Kind():                   {time.Hour, false}, // hourly + daily + monthly; shortest wins
		riverjobs.DelinquencyArgs{}.Kind():               {15 * time.Minute, false},
		riverjobs.CreditReconcileArgs{}.Kind():           {30 * time.Minute, false},
		riverjobs.NotificationEmailSweepArgs{}.Kind():    {10 * time.Minute, false},
	}
	rt := &Runtime{}
	jobs, err := rt.buildRiverPeriodicJobs(context.Background())
	require.NoError(t, err)
	runOnStart := map[string]bool{}
	for _, job := range jobs {
		args, opts := periodicArgs(job)
		require.NotNil(t, opts, args.Kind())
		require.Equal(t, riverjobs.QueueBilling, opts.Queue, args.Kind())
		require.NotZero(t, opts.UniqueOpts, "%s must coalesce overlapping ticks", args.Kind())
		runOnStart[args.Kind()] = runOnStart[args.Kind()] || periodicRunOnStart(job)
	}
	periods := rt.workerHealthRegistrations().Snapshot()
	require.Len(t, runOnStart, len(want))
	for kind, w := range want {
		require.Equal(t, w.runOnStart, runOnStart[kind], "%s RunOnStart", kind)
		require.Equal(t, w.period, periods[kind], "%s period", kind)
	}

	// catalog_reconciliation_interval: 0 disables, a value sets the cadence,
	// a typo refuses instead of silently picking a schedule.
	for raw, period := range map[string]time.Duration{"0": 0, "30m": 30 * time.Minute} {
		rt := &Runtime{Config: &config.Config{CatalogReconciliationInterval: raw}}
		_, err := rt.buildRiverPeriodicJobs(context.Background())
		require.NoError(t, err)
		got, scheduled := rt.workerHealthRegistrations().Snapshot()[riverjobs.CatalogReconciliationPullArgs{}.Kind()]
		require.Equal(t, period != 0, scheduled, raw)
		require.Equal(t, period, got, raw)
	}
	_, err = (&Runtime{Config: &config.Config{CatalogReconciliationInterval: "30minutes"}}).buildRiverPeriodicJobs(context.Background())
	require.Error(t, err)
}

func TestRuntimeWiringPolicies(t *testing.T) {
	rt := &Runtime{}
	require.Equal(t, config.RiverSchema, rt.riverSchemaOrDefault())
	rt.SetRiverSchema("  jobs ")
	require.Equal(t, "jobs", rt.riverSchemaOrDefault(), "direct River reads follow the bound client's schema")

	// Alert links go to the independently configured console, never the billing mount.
	cfg := &config.Config{PublicBillingBaseURL: "https://billing.example/billing"}
	require.Empty(t, alertingDashboardBaseURL(cfg))
	cfg.DashboardBaseURL = "https://console.example/admin/"
	require.Equal(t, "https://console.example/admin", alertingDashboardBaseURL(cfg))
}

// Devnet money is fake: a devnet deployment never depends on Hermes, while
// mainnet refuses to price an unknown feed. Neither path touches the network.
func TestPythPriceProviderDevnetParity(t *testing.T) {
	for _, sandbox := range []bool{true, false} {
		cfg := &config.Config{TestMode: config.CredentialPostureLive}
		if sandbox {
			cfg.TestMode = config.CredentialPostureSandbox
		}
		provider, err := createPythPriceProvider(cfg)
		require.NoError(t, err)
		price, err := provider.PriceUSD(context.Background(), "WEIRD")
		if sandbox {
			require.NoError(t, err)
			require.Equal(t, 1.0, price)
		} else {
			require.Error(t, err)
		}
	}
}

type closingControlPlane struct{ calls int }

func (c *closingControlPlane) Close() { c.calls++ }

func TestCloseReleasesControlPlaneOnce(t *testing.T) {
	cp := &closingControlPlane{}
	a := &App{}
	a.SetControlPlane(cp, nil)
	require.NoError(t, a.Close(context.Background()))
	require.NoError(t, a.Close(context.Background()))
	require.Equal(t, 1, cp.calls)
	require.Nil(t, a.ControlPlane)
	require.NoError(t, (*App)(nil).Close(context.Background()))
}
