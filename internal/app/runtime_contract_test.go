package app

import (
	"context"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/providerrecovery"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// periodicRunOnStart reads River's unexported opts; River exposes no accessor.
func periodicRunOnStart(job *river.PeriodicJob) bool {
	v := reflect.ValueOf(job).Elem().FieldByName("opts")
	opts := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(*river.PeriodicJobOpts)
	return opts != nil && opts.RunOnStart
}

func periodicSchedule(job *river.PeriodicJob) river.PeriodicSchedule {
	v := reflect.ValueOf(job).Elem().FieldByName("scheduleFunc")
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(river.PeriodicSchedule)
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
		riverjobs.ProviderRefreshArgs{}.Kind():           {providerrecovery.RefreshInterval, true},
		riverjobs.JobRescueArgs{}.Kind():                 {time.Minute, true},
		riverjobs.PriceMigrationRedriveArgs{}.Kind():     {time.Hour, true},
		riverjobs.AccountUpdaterBatchArgs{}.Kind():       {6 * time.Hour, true},
		riverjobs.MerchantSecretCleanupArgs{}.Kind():     {5 * time.Minute, true},
		riverjobs.ConvergeSweepArgs{}.Kind():             {15 * time.Minute, true},
		riverjobs.LedgerIntegrityArgs{}.Kind():           {24 * time.Hour, true},
		riverjobs.CleanupExpiredDataArgs{}.Kind():        {time.Hour, false},
		riverjobs.IdempotencyGCArgs{}.Kind():             {time.Hour, false},
		riverjobs.SolanaCrankArgs{}.Kind():               {time.Hour, false},
		riverjobs.SolanaGasAlertArgs{}.Kind():            {6 * time.Hour, false},
		riverjobs.SolanaReconcileArgs{}.Kind():           {6 * time.Hour, false},
		riverjobs.SolanaPayGCArgs{}.Kind():               {time.Hour, false},
		riverjobs.CreditExpiryArgs{}.Kind():              {time.Hour, false},
		riverjobs.BalanceWatchArgs{}.Kind():              {riverjobs.BalanceWatchInterval, true},
		riverjobs.AdmissionDenialFlushArgs{}.Kind():      {5 * time.Minute, false},
		riverjobs.CatalogReconciliationPullArgs{}.Kind(): {time.Hour, false},
		riverjobs.StripeWebhookReconcileArgs{}.Kind():    {time.Hour, false},
		riverjobs.UsageOrderArgs{}.Kind():                {time.Hour, true}, // hourly + daily + monthly; shortest wins
		riverjobs.DelinquencyArgs{}.Kind():               {15 * time.Minute, false},
		riverjobs.RebillWatchArgs{}.Kind():               {riverjobs.RebillWatchInterval, false},
		riverjobs.AttemptEnrichmentArgs{}.Kind():         {riverjobs.AttemptEnrichmentInterval, false},
		riverjobs.NMIHistoryArgs{}.Kind():                {riverjobs.NMIHistoryInterval, false},
		riverjobs.ChargeAttributionArgs{}.Kind():         {riverjobs.ChargeAttributionInterval, false},
		riverjobs.OrderExpiryArgs{}.Kind():               {5 * time.Minute, false},
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
		if usage, ok := args.(riverjobs.UsageOrderArgs); ok && usage.Collect {
			require.True(t, opts.UniqueOpts.ByArgs, "hourly and monthly phases must stay distinct")
			if usage.UseMonthlyFloor {
				require.Equal(t, 30*24*time.Hour, opts.UniqueOpts.ByPeriod, "completed monthly slot survives restarts")
			} else {
				require.Zero(t, opts.UniqueOpts.ByPeriod, "an unfinished hourly scan coalesces across hour boundaries")
				require.True(t, opts.UniqueOpts.ByQueue)
				require.ElementsMatch(t, []rivertype.JobState{
					rivertype.JobStateAvailable, rivertype.JobStatePending,
					rivertype.JobStateRunning, rivertype.JobStateRetryable,
					rivertype.JobStateScheduled,
				}, opts.UniqueOpts.ByState)
			}
		}
		// Leaders taking over at different instants of a period schedule its
		// next run at the same boundary, where the next uniqueness bucket starts.
		period := opts.UniqueOpts.ByPeriod
		if period == 0 {
			continue
		}
		boundary := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Truncate(period).Add(period)
		for _, takeover := range []time.Time{boundary.Add(-period), boundary.Add(-period / 2), boundary.Add(-time.Nanosecond)} {
			require.Equal(t, boundary, periodicSchedule(job).Next(takeover), "%s after a takeover at %s", args.Kind(), takeover)
		}
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
	require.Equal(t, "billing_river", rt.riverSchemaOrDefault(), "OpenRails' own River sits beside its schema")
	require.Equal(t, "shop_river", (&Runtime{Config: &config.Config{Database: config.DatabaseConfig{Schema: "Shop"}}}).riverSchemaOrDefault())
	require.Equal(t, "jobs", (&Runtime{Config: &config.Config{Database: config.DatabaseConfig{RiverSchema: " Jobs "}}}).riverSchemaOrDefault())
	rt.SetRiverSchema("  jobs ")
	require.Equal(t, "jobs", rt.riverSchemaOrDefault(), "direct River reads follow the bound client's schema")

	// Alert links go to the independently configured console, never the billing mount.
	cfg := &config.Config{PublicBillingBaseURL: "https://billing.example/billing"}
	require.Empty(t, alertingDashboardBaseURL(cfg))
	cfg.DashboardBaseURL = "https://console.example/admin/"
	require.Equal(t, "https://console.example/admin", alertingDashboardBaseURL(cfg))
}

// Devnet and testnet money is fake: a merchant whose Solana PSP is on either
// never depends on Hermes, while mainnet refuses to price an unknown feed.
// Neither path touches the network.
func TestPythPriceProviderDevnetParity(t *testing.T) {
	for _, network := range []string{config.EnvironmentMainnet, config.EnvironmentDevnet, config.EnvironmentTestnet} {
		sandbox := network != config.EnvironmentMainnet
		provider, err := createPythPriceProvider(func(context.Context) string { return network })
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
