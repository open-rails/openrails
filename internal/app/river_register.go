package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/intents"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// buildRiverWorkers constructs the worker registry for River.
func (r *Runtime) buildRiverWorkers(ctx context.Context) (*river.Workers, error) {
	workers := river.NewWorkers()
	if err := r.addBillingWorkersToRegistry(ctx, workers); err != nil {
		return nil, err
	}
	return workers, nil
}

// addBillingWorkersToRegistry adds billing workers to an existing worker registry.
// This is used both internally (buildRiverWorkers) and externally (AddBillingWorkersTo).
func (r *Runtime) addBillingWorkersToRegistry(ctx context.Context, workers *river.Workers) error {
	if err := r.validateBillingWorkerRuntime(); err != nil {
		return err
	}

	clock := r.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	// ONE intent registry instance feeds the scheduled executor/verifier, the
	// dunning worker's synchronous rebill path and (via Runtime.IntentRunner)
	// the admin refund producer — per-type semantics can never diverge.
	intentRegistry := r.buildIntentRegistry(clock)
	if err := river.AddWorkerSafely(workers, &riverjobs.DunningWorker{DB: r.DB, Config: r.Config, Clock: clock, NMIClients: r.NMIClients, EventLogService: r.EventLogService, IdempotencyService: r.IdempotencyService, DeferDelete: r.DeferredDeletes, Intents: r.intentRunner(intentRegistry, clock)}); err != nil {
		return fmt.Errorf("add dunning worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.IdempotencyCleanupWorker{Config: r.Config}); err != nil {
		return fmt.Errorf("add idempotency cleanup worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CCBillReconcileWorker{DB: r.DB, DataLink: r.CCBillDataLink, NotificationService: r.NotificationService}); err != nil {
		return fmt.Errorf("add ccbill reconcile worker: %w", err)
	}
	// Webhook processing is now synchronous-only - no background workers needed.
	// Payment processors (CCBill, NMI) retry failed webhooks from their end.
	if err := river.AddWorkerSafely(workers, &riverjobs.CleanupExpiredDataWorker{
		DB:     r.DB,
		Clock:  clock,
		Config: riverjobs.DefaultCleanupConfig(),
	}); err != nil {
		return fmt.Errorf("add cleanup expired data worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CreditExpiryWorker{
		DB:     r.DB,
		Config: r.Config,
		Clock:  clock,
	}); err != nil {
		return fmt.Errorf("add credit expiry worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.HoldExpiryWorker{
		DB:     r.DB,
		Config: r.Config,
		Clock:  clock,
	}); err != nil {
		return fmt.Errorf("add hold expiry worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CancelSubscriptionWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		UserSubscriptionService:      r.UserSubscriptionService,
		SubscriptionService:          r.SubscriptionService,
		SubscriptionLifecycleService: r.SubscriptionLifecycleService,
	}); err != nil {
		return fmt.Errorf("add cancel subscription worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.ResumeSubscriptionWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		EntitlementService:           r.EntitlementService,
		SubscriptionService:          r.SubscriptionService,
		SubscriptionLifecycleService: r.SubscriptionLifecycleService,
		NMIClients:                   r.NMIClients,
	}); err != nil {
		return fmt.Errorf("add resume subscription worker: %w", err)
	}
	// Provider intent ledger (#358): the executor drains due outbound provider
	// mutations (deferred NMI deletes, refunds, manual rebills), the verifier
	// resolves ambiguous outcomes via provider reads. These replaced the
	// NMIDeleteSubscription worker + boot rescan.
	if err := river.AddWorkerSafely(workers, &riverjobs.ProviderIntentExecuteWorker{
		DB:       r.DB,
		Config:   r.Config,
		Clock:    clock,
		Registry: intentRegistry,
	}); err != nil {
		return fmt.Errorf("add provider intent execute worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.ProviderIntentVerifyWorker{
		DB:       r.DB,
		Config:   r.Config,
		Clock:    clock,
		Registry: intentRegistry,
	}); err != nil {
		return fmt.Errorf("add provider intent verify worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.WebhookProcessWorker{
		Dispatcher: r.WebhookDispatcher,
	}); err != nil {
		return fmt.Errorf("add webhook process worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CatalogReconciliationPullWorker{
		DB:         r.DB,
		Config:     r.Config,
		NMIClients: r.NMIClients,
	}); err != nil {
		return fmt.Errorf("add catalog reconciliation worker: %w", err)
	}
	// Credit money-in + reconciliation workers (#239/#240/#241/#243). The
	// auto-top-up and arrears workers carry no Charger and the low-balance worker
	// no Alerter yet, so they log-and-skip until the processor/notification wiring
	// lands; the reconcile worker runs fully (alert-only).
	if err := river.AddWorkerSafely(workers, &riverjobs.LowBalanceAlertWorker{
		Credits: r.CreditsService,
	}); err != nil {
		return fmt.Errorf("add low-balance alert worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.AutoTopupWorker{
		Credits: r.CreditsService,
		Config:  r.Config,
	}); err != nil {
		return fmt.Errorf("add auto-topup worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.ArrearsChargeWorker{
		Credits: r.CreditsService,
		Config:  r.Config,
	}); err != nil {
		return fmt.Errorf("add arrears charge worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.CreditReconcileWorker{
		Credits: r.CreditsService,
		Clock:   clock,
	}); err != nil {
		return fmt.Errorf("add credit reconcile worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &riverjobs.InvoiceFinalizeWorker{
		Credits: r.CreditsService,
		Clock:   clock,
	}); err != nil {
		return fmt.Errorf("add invoice finalize worker: %w", err)
	}
	// Solana recurring cranker (#256). The Cranker (per-tenant signer + RPC) is
	// wired once tenant Solana signing lands; until then it log-and-skips like the
	// money-in workers above. Lifecycle is wired so renewals + dunning route
	// correctly the moment the Cranker is connected.
	solanaCrankWorker := &riverjobs.SolanaCrankWorker{
		DB:        r.DB,
		Config:    r.Config,
		Clock:     clock,
		Lifecycle: r.SubscriptionLifecycleService,
	}
	if r.SolanaCranker != nil {
		solanaCrankWorker.Cranker = r.SolanaCranker
	}
	if err := river.AddWorkerSafely(workers, solanaCrankWorker); err != nil {
		return fmt.Errorf("add solana cranker worker: %w", err)
	}
	// Solana cranker-wallet gas-float alert (#258): warns when a tenant's cranker
	// wallet is low on SOL. Alert-only, no auto-top-up.
	if err := river.AddWorkerSafely(workers, &riverjobs.SolanaGasAlertWorker{
		DB:  r.DB,
		RPC: r.SolanaRPC,
	}); err != nil {
		return fmt.Errorf("add solana gas alert worker: %w", err)
	}
	// Solana ledger reconciliation (#258): cross-checks confirmed on-chain pulls
	// against billing.payments and raises operator repair alerts on drift.
	if err := river.AddWorkerSafely(workers, &riverjobs.SolanaReconcileWorker{
		DB:                  r.DB,
		NotificationService: r.NotificationService,
		Clock:               clock,
	}); err != nil {
		return fmt.Errorf("add solana reconcile worker: %w", err)
	}
	return nil
}

// buildIntentRegistry assembles the per-type intent semantics for the
// provider intent executor/verifier (#358): deferred NMI deletes (phase A),
// NMI/Stripe refunds (phase B), manual rebills (phase C) and catalog archive
// ops — Stripe product/price archives + Solana plan sunsets (phase D).
func (r *Runtime) buildIntentRegistry(clock clockwork.Clock) *intents.Registry {
	return intents.NewRegistry(
		intents.NewNMIDeleteHandler(r.DB, r.Config, r.NMIClients, clock),
		intents.NewNMIRefundHandler(r.DB, r.NMIClients, clock),
		intents.NewStripeRefundHandler(r.DB, r.Config, clock),
		intents.NewManualRebillHandler(r.DB, r.Config, r.NMIClients, clock, r.EventLogService),
		intents.NewStripeArchiveProductHandler(r.DB, r.Config, clock),
		intents.NewStripeArchivePriceHandler(r.DB, r.Config, clock),
		intents.NewSolanaSunsetPlanHandler(r.DB, r.SolanaPlanService, r.SolanaRPC, clock),
	)
}

// intentRunner builds a Runner over a registry. Config is attached only when
// non-nil so the origin x mode gate's nil check (= full mode in tests) keeps
// working — a typed-nil ModeView would panic inside the gate.
func (r *Runtime) intentRunner(registry *intents.Registry, clock clockwork.Clock) *intents.Runner {
	runner := &intents.Runner{
		Store:    intents.NewStore(r.DB),
		Registry: registry,
		Clock:    clock,
	}
	if r.Config != nil {
		runner.Config = r.Config
	}
	return runner
}

// IntentRunner returns a Runner for synchronous enqueue+execute from request
// paths (the admin refund producer). The registry is assembled fresh from the
// runtime's live dependencies — same constructor set as the scheduled
// workers, so semantics are identical.
func (r *Runtime) IntentRunner() *intents.Runner {
	clock := r.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	return r.intentRunner(r.buildIntentRegistry(clock), clock)
}

// catalogReconciliationInterval returns the schedule for the alert-only Stripe
// catalog reconciliation loop (issue #209). Configurable via the
// OPENRAILS_CATALOG_RECONCILIATION_INTERVAL env var (Go duration, e.g. "30m",
// "2h"). Defaults to 1h. A value of "0" (or "0s") disables the loop entirely;
// the returned ok=false signals the caller to skip scheduling it.
func catalogReconciliationInterval() (interval time.Duration, ok bool) {
	raw := strings.TrimSpace(os.Getenv("OPENRAILS_CATALOG_RECONCILIATION_INTERVAL"))
	if raw == "" {
		return time.Hour, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		// Unparseable -> fall back to the safe default rather than disabling.
		return time.Hour, true
	}
	if d <= 0 {
		return 0, false // interval=0 disables the loop
	}
	return d, true
}

func (r *Runtime) validateBillingWorkerRuntime() error {
	if r == nil {
		return fmt.Errorf("runtime is required")
	}
	if r.DB == nil {
		return fmt.Errorf("billing worker runtime DB is required")
	}
	if r.Config == nil {
		return fmt.Errorf("billing worker runtime config is required")
	}
	if r.SubscriptionService == nil {
		return fmt.Errorf("billing worker runtime subscription service is required")
	}
	if r.UserSubscriptionService == nil {
		return fmt.Errorf("billing worker runtime user subscription service is required")
	}
	if r.SubscriptionLifecycleService == nil {
		return fmt.Errorf("billing worker runtime subscription lifecycle service is required")
	}
	if r.EntitlementService == nil {
		return fmt.Errorf("billing worker runtime entitlement service is required")
	}
	if r.WebhookDispatcher == nil {
		return fmt.Errorf("billing worker runtime webhook dispatcher is required")
	}
	return nil
}

// buildRiverPeriodicJobs defines recurring schedules for workers using River periodic jobs.
func (r *Runtime) buildRiverPeriodicJobs(ctx context.Context) ([]*river.PeriodicJob, error) {
	var jobs []*river.PeriodicJob

	// Every 4 hours: run Dunning worker to process past_due subscriptions
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(4*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.DunningArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 4 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Daily: Idempotency cleanup
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(24*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.IdempotencyCleanupArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 24 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Every 6 hours: CCBill reconcile
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(6*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.CCBillReconcileArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 6 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Webhook retry job removed - webhooks are now processed synchronously only.
	// Payment processors (CCBill, NMI) will retry failed webhooks from their end.

	// Every minute: drain due provider intents (#358 — the ACTION pipeline;
	// deliberately scheduled, unlike reconcile runs which stay manual).
	// RunOnStart drains parked/overdue intents right after boot — when a mode
	// change, kill-switch flip or restart is exactly what unblocked them —
	// replacing the retired #344 boot rescan.
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.ProviderIntentExecuteArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Every 5 minutes: resolve unknown_needs_verify intents via provider
	// reads (#358 verifier) before any retry.
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.ProviderIntentVerifyArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 5 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every hour: cleanup expired data (wallet challenges, payment intents, etc.)
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.CleanupExpiredDataArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every hour: crank due Solana recurring subscriptions (#256). Worker frequency
	// is decoupled from the monthly billing cadence — the due-query (next_pull_at)
	// filters to what's actually due.
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.SolanaCrankArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 6 hours: alert on low Solana cranker-wallet SOL gas (#258).
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(6*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.SolanaGasAlertArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 6 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 6 hours: reconcile confirmed Solana pulls against the ledger (#258).
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(6*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.SolanaReconcileArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 6 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every hour: expire credit batches
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.CreditExpiryArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 5 minutes: expire orphaned credit holds
	// Handles cases where jobs crash without calling capture/release
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.HoldExpiryArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 5 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Catalog reconciliation loop (issue #209): pull the Stripe catalog and
	// diff it against the OpenRails DB, recording drift + orphan events.
	// Alert-only — never mutates Stripe or the catalog rows. Interval is
	// configurable via OPENRAILS_CATALOG_RECONCILIATION_INTERVAL (0 disables).
	if interval, ok := catalogReconciliationInterval(); ok {
		jobs = append(jobs, river.NewPeriodicJob(
			river.PeriodicInterval(interval),
			func() (river.JobArgs, *river.InsertOpts) {
				return riverjobs.CatalogReconciliationPullArgs{}, &river.InsertOpts{
					Queue:      riverjobs.QueueBilling,
					UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: interval},
				}
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// Every hour: low-balance alerts (#240) + arrears collection (#241).
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.LowBalanceAlertArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.ArrearsChargeArgs{ThresholdMicros: riverjobs.ArrearsHourlyThresholdMicros}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))
	// Monthly arrears sweep (#301): collect the long tail of small owed balances
	// (>= $1 floor) the hourly threshold trigger leaves behind. "Whichever comes
	// first." Idempotent per owed-snapshot so it never double-charges what the
	// hourly job already collected.
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(30*24*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.ArrearsChargeArgs{ThresholdMicros: riverjobs.ArrearsMonthlyFloorMicros}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 30 * 24 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Daily: finalize the previous calendar month's itemized invoices (#303).
	// Idempotent per (owner, credit_type, period), so a daily run reliably
	// finalizes the prior month shortly after rollover and no-ops thereafter.
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(24*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.InvoiceFinalizeArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 24 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 15 minutes: prepaid auto-top-up (#239).
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(15*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.AutoTopupArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 15 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 30 minutes: credit ledger reconciliation (#243, alert-only).
	jobs = append(jobs, river.NewPeriodicJob(
		river.PeriodicInterval(30*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.CreditReconcileArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 30 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	return jobs, nil
}
