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
	"github.com/open-rails/openrails/internal/modules/checkout"
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
	// #699: workers that resolve per-merchant secrets (provider refresh, Stripe
	// webhook reconcile) need the merchants service even on embedded hosts that
	// never build the standalone HTTP server. No-op when already set.
	r.EnsureMerchantsService(ctx)

	clock := r.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	// ONE intent registry instance feeds the scheduled executor/verifier, the
	// dunning worker's synchronous rebill path and (via Runtime.IntentRunner)
	// the admin refund producer — per-type semantics can never diverge.
	intentRegistry := r.buildIntentRegistry(clock)
	if err := addTrackedWorker(r, workers, &riverjobs.DunningWorker{DB: r.DB, Config: r.Config, Rails: r.Rails, Clock: clock, NMIClients: r.NMIClients, EventLogService: r.EventLogService, IdempotencyService: r.IdempotencyService, DeferDelete: r.DeferredDeletes, Intents: r.intentRunner(intentRegistry, clock)}); err != nil {
		return fmt.Errorf("add dunning worker: %w", err)
	}
	// Provider Refresh (#574): always-on provider truth refresh. It wraps
	// bounded provider event pulls, the unknown-cohort reconcile (#632/#665 —
	// the one per-subscription verification path; the #367 liveness worker is
	// retired), CCBill DataLink, and scoped convergence after refresh writes.
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderRefreshWorker{
		DB:                  r.DB,
		Config:              r.Config,
		Rails:               r.Rails,
		Clock:               clock,
		Merchants:           r.Merchants, // #699: per-merchant store-armed pulls
		NMIClients:          r.NMIClients,
		CCBillDataLink:      r.CCBillDataLink,
		SolanaRPC:           r.SolanaRPC,
		EventLogService:     r.EventLogService,
		DeferDelete:         r.DeferredDeletes,
		NotificationService: r.NotificationService,
	}); err != nil {
		return fmt.Errorf("add provider refresh worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.CCBillReconcileWorker{DB: r.DB, DataLink: r.CCBillDataLink, NotificationService: r.NotificationService}); err != nil {
		return fmt.Errorf("add ccbill reconcile worker: %w", err)
	}
	// Webhook processing is now synchronous-only - no background workers needed.
	// Payment rails (CCBill, NMI) retry failed webhooks from their end.
	if err := addTrackedWorker(r, workers, &riverjobs.CleanupExpiredDataWorker{
		DB:     r.DB,
		Clock:  clock,
		Config: riverjobs.DefaultCleanupConfig(),
	}); err != nil {
		return fmt.Errorf("add cleanup expired data worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.CreditExpiryWorker{
		DB:    r.DB,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add credit expiry worker: %w", err)
	}
	// Convergence Engine sweep (#511): periodically run reconcile.Converge for
	// every active merchant, catching internal-plane drift (stalled dunning,
	// elapsed grace, abandoned checkouts, unmaterialized grant effects) that no
	// inline mutation touched. The background twin of the inline Converge hooks.
	if err := addTrackedWorker(r, workers, &riverjobs.ConvergeSweepWorker{
		DB:     r.DB,
		Config: r.Config,
		Clock:  clock,
	}); err != nil {
		return fmt.Errorf("add converge sweep worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.CancelSubscriptionWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		Rails:                        r.Rails,
		UserSubscriptionService:      r.UserSubscriptionService,
		SubscriptionService:          r.SubscriptionService,
		SubscriptionLifecycleService: r.SubscriptionLifecycleService,
	}); err != nil {
		return fmt.Errorf("add cancel subscription worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.ResumeSubscriptionWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		Rails:                        r.Rails,
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
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderIntentExecuteWorker{
		DB:             r.DB,
		Config:         r.Config,
		Clock:          clock,
		Registry:       intentRegistry,
		MutationLogger: intents.NewAnalyticsMutationLogger(r.EventLogService),
	}); err != nil {
		return fmt.Errorf("add provider intent execute worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderIntentVerifyWorker{
		DB:             r.DB,
		Config:         r.Config,
		Clock:          clock,
		Registry:       intentRegistry,
		MutationLogger: intents.NewAnalyticsMutationLogger(r.EventLogService),
	}); err != nil {
		return fmt.Errorf("add provider intent verify worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.WebhookProcessWorker{
		Dispatcher: r.WebhookDispatcher,
	}); err != nil {
		return fmt.Errorf("add webhook process worker: %w", err)
	}
	// #684: webhook wake-ups — the coalesced per-subscription fetch-and-converge
	// job the slimmed Stripe/NMI subscription-state handlers enqueue.
	if err := addTrackedWorker(r, workers, &riverjobs.SubscriptionConvergeWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		Rails:                        r.Rails,
		Clock:                        clock,
		NMIClients:                   r.NMIClients,
		PriceService:                 r.PriceService,
		ProductService:               r.ProductService,
		SubscriptionService:          r.SubscriptionService,
		SubscriptionLifecycleService: r.SubscriptionLifecycleService,
		PaymentService:               r.PaymentService,
		MoneyService:                 r.MoneyService,
		NotificationService:          r.NotificationService,
		RailCustomerService:          r.RailCustomerService,
		CheckoutSessionService:       r.CheckoutSessionService,
	}); err != nil {
		return fmt.Errorf("add subscription converge worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.CatalogReconciliationPullWorker{
		DB:         r.DB,
		Config:     r.Config,
		Rails:      r.Rails,
		NMIClients: r.NMIClients,
	}); err != nil {
		return fmt.Errorf("add catalog reconciliation worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.StripeWebhookReconcileWorker{
		DB:        r.DB,
		Config:    r.Config,
		Rails:     r.Rails,
		Merchants: r.Merchants,
	}); err != nil {
		return fmt.Errorf("add stripe webhook reconcile worker: %w", err)
	}
	// Credit money-in + reconciliation workers (#239/#241/#243/#508). The
	// auto-top-up and invoice workers share the configured off-session charger;
	// when it is nil they log-and-skip until rail wiring is attached.
	// LowBalanceAlertWorker (#240) is NOT registered: no money.Alerter
	// implementation exists in the runtime, so registration was a permanent
	// no-op (#673) — re-add it together with the notification wiring.
	if err := addTrackedWorker(r, workers, &riverjobs.AutoTopupWorker{
		DB:      r.DB,
		Money:   r.MoneyService,
		Config:  r.Config,
		Intents: r.intentRunner(intentRegistry, clock),
	}); err != nil {
		return fmt.Errorf("add auto-topup worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.InvoiceWorker{
		DB:      r.DB,
		Money:   r.MoneyService,
		Charger: r.MoneyCharger,
		Config:  r.Config,
		Clock:   clock,
	}); err != nil {
		return fmt.Errorf("add invoice worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.CreditReconcileWorker{
		Money: r.MoneyService,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add credit reconcile worker: %w", err)
	}
	// Solana recurring cranker (#256). The Cranker (per-merchant signer + RPC) is
	// wired once merchant Solana signing lands; until then it log-and-skips like the
	// money-in workers above. Lifecycle is wired so renewals + dunning route
	// correctly the moment the Cranker is connected.
	solanaCrankWorker := &riverjobs.SolanaCrankWorker{
		DB:        r.DB,
		Config:    r.Config,
		Clock:     clock,
		Lifecycle: r.SubscriptionLifecycleService,
		// #674: pulls run as durable solana_pull intents through the same
		// registry the scheduled executor/verifier drains.
		Intents: r.intentRunner(intentRegistry, clock),
	}
	if r.SolanaCranker != nil {
		solanaCrankWorker.Cranker = r.SolanaCranker
	}
	if err := addTrackedWorker(r, workers, solanaCrankWorker); err != nil {
		return fmt.Errorf("add solana cranker worker: %w", err)
	}
	// Solana cranker-wallet gas-float alert (#258): warns when a merchant's cranker
	// wallet is low on SOL. Alert-only, no auto-top-up.
	if err := addTrackedWorker(r, workers, &riverjobs.SolanaGasAlertWorker{
		DB:  r.DB,
		RPC: r.SolanaRPC,
	}); err != nil {
		return fmt.Errorf("add solana gas alert worker: %w", err)
	}
	// Solana ledger reconciliation (#258): cross-checks confirmed on-chain pulls
	// against openrails.payments and raises operator repair alerts on drift.
	if err := addTrackedWorker(r, workers, &riverjobs.SolanaReconcileWorker{
		DB:                  r.DB,
		NotificationService: r.NotificationService,
		Clock:               clock,
	}); err != nil {
		return fmt.Errorf("add solana reconcile worker: %w", err)
	}
	// Worker health checker (#689): evaluates per-kind health rows written by the
	// WorkerHealthMiddleware and raises repair alerts for never-succeeded /
	// failing / stale kinds. Registered last so every kind above is already noted.
	if err := addTrackedWorker(r, workers, &riverjobs.WorkerHealthCheckWorker{
		DB:                  r.DB,
		Clock:               clock,
		Registrations:       r.workerHealthRegistrations(),
		NotificationService: r.NotificationService,
	}); err != nil {
		return fmt.Errorf("add worker health check worker: %w", err)
	}
	return nil
}

// buildIntentRegistry assembles the per-type intent semantics for the
// provider intent executor/verifier (#358): deferred NMI deletes (phase A),
// NMI/Stripe refunds (phase B), manual rebills (phase C), catalog archive
// ops — Stripe product/price archives + Solana plan sunsets (phase D) — and
// the #674 write-through kinds (checkout NMI sales, auto-top-up charges,
// Solana recurring pulls).
func (r *Runtime) buildIntentRegistry(clock clockwork.Clock) *intents.Registry {
	registry := intents.NewRegistry(
		intents.NewNMIDeleteHandler(r.DB, r.Config, r.NMIClients, clock),
		intents.NewCCBillCancelHandler(r.DB, r.CCBillDataLink, clock), // #696 (nil client parks)
		intents.NewNMIRefundHandler(r.DB, r.NMIClients, clock),
		intents.NewStripeRefundHandler(r.DB, r.Config, r.Rails, clock),
		intents.NewManualRebillHandler(r.DB, r.Config, r.NMIClients, clock, r.EventLogService),
		intents.NewStripeArchiveProductHandler(r.DB, r.Config, r.Rails, clock),
		intents.NewStripeArchivePriceHandler(r.DB, r.Config, r.Rails, clock),
		intents.NewSolanaSunsetPlanHandler(r.DB, r.SolanaPlanService, r.SolanaRPC, clock),
	)
	// #674 write-through kinds live next to their domain services and register
	// only when those services are wired (worker-only runtimes may lack them).
	if r.CheckoutService != nil {
		if r.CheckoutService.NMISaleService != nil {
			registry.Register(checkout.NewNMISaleIntentHandler(r.CheckoutService.NMISaleService))
		}
		registry.Register(checkout.NewNMISubscriptionCreateIntentHandler(r.CheckoutService))
	}
	// #674 tail: durable user-initiated vault deletes (an unwired VaultService
	// resolves no client, so the handler parks — never fails).
	if r.VaultService != nil {
		registry.Register(intents.NewNMIVaultDeleteHandler(r.DB, r.VaultService))
	}
	registry.Register(intents.NewTopupChargeHandler(r.DB, r.MoneyCharger, r.NMIClients, clock))
	// Solana recurring pull (#674): the handler wraps the crank state machine
	// with the pre-submit signature write-ahead + chain-read verification. The
	// core worker here carries NO Intents runner (the handler IS the execution
	// path; a runner on it would recurse).
	solanaPullCore := &riverjobs.SolanaCrankWorker{
		DB:        r.DB,
		Config:    r.Config,
		Clock:     clock,
		Lifecycle: r.SubscriptionLifecycleService,
	}
	if r.SolanaCranker != nil {
		solanaPullCore.Cranker = r.SolanaCranker
	}
	var solanaChain riverjobs.SolanaTxReader
	if r.SolanaRPC != nil {
		solanaChain = r.SolanaRPC
	}
	registry.Register(riverjobs.NewSolanaPullIntentHandler(solanaPullCore, intents.NewStore(r.DB), solanaChain))
	return registry
}

// intentRunner builds a Runner over a registry. Config is attached only when
// non-nil so the origin x mode gate's nil check (= full mode in tests) keeps
// working — a typed-nil ModeView would panic inside the gate.
func (r *Runtime) intentRunner(registry *intents.Registry, clock clockwork.Clock) *intents.Runner {
	runner := &intents.Runner{
		Store:    intents.NewStore(r.DB),
		Logger:   intents.NewAnalyticsMutationLogger(r.EventLogService),
		Registry: registry,
		Breaker:  intents.NewVolumeBreaker(r.DB), // #679: gate destructive types everywhere
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
	jobs = append(jobs, r.healthPeriodic(
		4*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.DunningArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 4 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 4 hours: Provider Refresh (#574) — refresh provider truth via
	// bounded event windows, the unknown-cohort reconcile, and CCBill DataLink.
	// RunOnStart=true: startup after a stale dump/outage should not wait for
	// the first 4-hour tick.
	jobs = append(jobs, r.healthPeriodic(
		4*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.ProviderRefreshArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 4 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Webhook retry job removed - webhooks are now processed synchronously only.
	// Payment rails (CCBill, NMI) will retry failed webhooks from their end.

	// Every minute: drain due provider intents (#358 — the ACTION pipeline;
	// deliberately scheduled, unlike reconcile runs which stay manual).
	// RunOnStart drains parked/overdue intents right after boot — when a mode
	// change, kill-switch flip or restart is exactly what unblocked them —
	// replacing the retired #344 boot rescan.
	jobs = append(jobs, r.healthPeriodic(
		time.Minute,
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
	jobs = append(jobs, r.healthPeriodic(
		5*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.ProviderIntentVerifyArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 5 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every hour: cleanup expired data (wallet challenges, payment intents, etc.)
	jobs = append(jobs, r.healthPeriodic(
		time.Hour,
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
	jobs = append(jobs, r.healthPeriodic(
		time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.SolanaCrankArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 6 hours: alert on low Solana cranker-wallet SOL gas (#258).
	jobs = append(jobs, r.healthPeriodic(
		6*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.SolanaGasAlertArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 6 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 6 hours: reconcile confirmed Solana pulls against the ledger (#258).
	jobs = append(jobs, r.healthPeriodic(
		6*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.SolanaReconcileArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 6 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every hour: expire credit batches
	jobs = append(jobs, r.healthPeriodic(
		time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.CreditExpiryArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Catalog reconciliation loop (issue #209): pull the Stripe catalog and
	// diff it against the OpenRails DB, recording drift + orphan events.
	// Alert-only — never mutates Stripe or the catalog rows. Interval is
	// configurable via OPENRAILS_CATALOG_RECONCILIATION_INTERVAL (0 disables).
	if interval, ok := catalogReconciliationInterval(); ok {
		jobs = append(jobs, r.healthPeriodic(
			interval,
			func() (river.JobArgs, *river.InsertOpts) {
				return riverjobs.CatalogReconciliationPullArgs{}, &river.InsertOpts{
					Queue:      riverjobs.QueueBilling,
					UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: interval},
				}
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}
	jobs = append(jobs, r.healthPeriodic(
		time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.StripeWebhookReconcileArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every hour: invoice collection (#241). (Low-balance alert scheduling was
	// removed with its worker registration — no Alerter implementation exists.)
	jobs = append(jobs, r.healthPeriodic(
		time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			args := riverjobs.InvoiceArgs{Collect: true}
			opts := &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: time.Hour},
			}
			return args, opts
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))
	// Monthly invoice sweep (#301): collect the long tail above the merchant's
	// floor that the hourly threshold trigger leaves behind.
	jobs = append(jobs, r.healthPeriodic(
		30*24*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			args := riverjobs.InvoiceArgs{
				Collect:         true,
				UseMonthlyFloor: true,
			}
			opts := &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 30 * 24 * time.Hour},
			}
			return args, opts
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Daily: finalize the previous merchant-configured itemized invoice period
	// (#303). Idempotent per (owner, credit_type, period).
	jobs = append(jobs, r.healthPeriodic(
		24*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.InvoiceArgs{FinalizePreviousMonth: true}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 24 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 15 minutes: prepaid auto-top-up (#239).
	jobs = append(jobs, r.healthPeriodic(
		15*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.AutoTopupArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 15 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 30 minutes: credit ledger reconciliation (#243, alert-only).
	jobs = append(jobs, r.healthPeriodic(
		30*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.CreditReconcileArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 30 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 15 minutes: Convergence Engine sweep (#511) — run reconcile.Converge
	// for every active merchant to remediate internal-plane drift (stalled
	// dunning, elapsed grace, abandoned checkouts, unmaterialized grant effects).
	// RunOnStart=true: a reboot after downtime is exactly when accumulated drift
	// is largest, and a clean merchant sweep is a cheap no-op.
	jobs = append(jobs, r.healthPeriodic(
		15*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.ConvergeSweepArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 15 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Every 5 minutes: worker health check (#689). RunOnStart=true so health rows
	// are seeded (and lingering incidents re-evaluated) right after boot.
	jobs = append(jobs, r.healthPeriodic(
		5*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.WorkerHealthCheckArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 5 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	return jobs, nil
}
