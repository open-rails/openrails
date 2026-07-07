package app

import (
	"context"
	"fmt"
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
	// Standalone: per-merchant refresh jobs land on the dedicated bounded queue
	// (#719) configured in buildRiverClient.
	if err := r.addBillingWorkersToRegistry(ctx, workers, riverjobs.QueueProviderRefresh); err != nil {
		return nil, err
	}
	return workers, nil
}

// addBillingWorkersToRegistry adds billing workers to an existing worker registry.
// This is used both internally (buildRiverWorkers) and externally (AddBillingWorkersTo).
// merchantRefreshQueue routes the #719 per-merchant refresh jobs: the bounded
// QueueProviderRefresh in standalone, QueueBilling for embedded hosts (whose
// river clients only configure that queue).
func (r *Runtime) addBillingWorkersToRegistry(ctx context.Context, workers *river.Workers, merchantRefreshQueue string) error {
	if err := r.validateBillingWorkerRuntime(); err != nil {
		return err
	}
	// #699: workers that resolve per-merchant secrets (provider refresh, Stripe
	// webhook reconcile) need the merchants service even on embedded hosts that
	// never build the standalone HTTP server. No-op when already set. Outside
	// development a failure to arm is now a boot error (#748) that propagates
	// through InitRiver -> RunWorkers, which main.go already treats as fatal.
	if err := r.EnsureMerchantsService(ctx); err != nil {
		return fmt.Errorf("arm merchants service: %w", err)
	}

	clock := r.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	// ONE intent registry instance feeds the scheduled executor/verifier, the
	// dunning worker's synchronous rebill path and (via Runtime.IntentRunner)
	// the admin refund producer — per-type semantics can never diverge.
	intentRegistry := r.buildIntentRegistry(clock)
	if err := addTrackedWorker(r, workers, &riverjobs.DunningWorker{DB: r.DB, Config: r.Config, Clock: clock, NMIResolver: r.CollectionResolver, IdempotencyService: r.IdempotencyService, DeferDelete: r.DeferredDeletes, Intents: r.intentRunner(intentRegistry, clock)}); err != nil {
		return fmt.Errorf("add dunning worker: %w", err)
	}
	// Provider Refresh (#574/#719): the 4h periodic kind is a SCHEDULER that
	// fans out one per-merchant refresh job (staggered; unique per merchant),
	// skipping merchants with no declared rail accounts.
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderRefreshSchedulerWorker{
		DB:            r.DB,
		Config:        r.Config,
		Clock:         clock,
		MerchantQueue: merchantRefreshQueue,
	}); err != nil {
		return fmt.Errorf("add provider refresh scheduler worker: %w", err)
	}
	// The per-merchant body: bounded provider event pulls, the unknown-cohort
	// reconcile (#632/#665 — the one per-subscription verification path; the
	// #367 liveness worker is retired), CCBill DataLink, and scoped
	// convergence after refresh writes.
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderRefreshWorker{
		DB:                  r.DB,
		Config:              r.Config,
		Clock:               clock,
		Merchants:           r.Merchants, // #699/#788: per-merchant store-armed pulls
		DeferDelete:         r.DeferredDeletes,
		NotificationService: r.NotificationService,
		Alerts:              r.AlertService, // #787: requires_review findings -> operator notifications
	}); err != nil {
		return fmt.Errorf("add provider refresh worker: %w", err)
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
	// #733: flush the Redis admission-denial counters to PG hourly aggregates.
	// Redis may be nil (no-admission deployments); the worker no-ops then.
	if err := addTrackedWorker(r, workers, &riverjobs.AdmissionDenialFlushWorker{
		DB:    r.DB,
		Redis: r.RedisClient,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add admission denial flush worker: %w", err)
	}
	// Convergence Engine sweep (#511): periodically run reconcile.Converge for
	// every active merchant, catching internal-plane drift (stalled dunning,
	// elapsed grace, abandoned checkouts, unmaterialized grant effects) that no
	// inline mutation touched. The background twin of the inline Converge hooks.
	if err := addTrackedWorker(r, workers, &riverjobs.ConvergeSweepWorker{
		DB:     r.DB,
		Config: r.Config,
		Clock:  clock,
		Alerts: r.AlertService, // #787: requires_review findings -> operator notifications
	}); err != nil {
		return fmt.Errorf("add converge sweep worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.CancelSubscriptionWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		Rails:                        r.RailConfigs,
		UserSubscriptionService:      r.UserSubscriptionService,
		SubscriptionService:          r.SubscriptionService,
		SubscriptionLifecycleService: r.SubscriptionLifecycleService,
	}); err != nil {
		return fmt.Errorf("add cancel subscription worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.ResumeSubscriptionWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		Rails:                        r.RailConfigs,
		EntitlementService:           r.EntitlementService,
		SubscriptionService:          r.SubscriptionService,
		SubscriptionLifecycleService: r.SubscriptionLifecycleService,
	}); err != nil {
		return fmt.Errorf("add resume subscription worker: %w", err)
	}
	// Provider intent ledger (#358): the executor drains due outbound provider
	// mutations (deferred NMI deletes, refunds, manual rebills), the verifier
	// resolves ambiguous outcomes via provider reads. These replaced the
	// NMIDeleteSubscription worker + boot rescan.
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderIntentExecuteWorker{
		DB:       r.DB,
		Config:   r.Config,
		Clock:    clock,
		Registry: intentRegistry,
	}); err != nil {
		return fmt.Errorf("add provider intent execute worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderIntentVerifyWorker{
		DB:       r.DB,
		Config:   r.Config,
		Clock:    clock,
		Registry: intentRegistry,
	}); err != nil {
		return fmt.Errorf("add provider intent verify worker: %w", err)
	}
	// #684: webhook wake-ups — the coalesced per-subscription fetch-and-converge
	// job the slimmed Stripe/NMI subscription-state handlers enqueue.
	if err := addTrackedWorker(r, workers, &riverjobs.SubscriptionConvergeWorker{
		DB:                           r.DB,
		Config:                       r.Config,
		Rails:                        r.RailConfigs,
		Clock:                        clock,
		NMIResolver:                  r.CollectionResolver,
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
		DB:          r.DB,
		Config:      r.Config,
		Rails:       r.RailConfigs,
		NMIResolver: r.CollectionResolver,
	}); err != nil {
		return fmt.Errorf("add catalog reconciliation worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.StripeWebhookReconcileWorker{
		DB:        r.DB,
		Config:    r.Config,
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
		RPC: r.SolanaRPCResolver.ChainReader(),
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
	// Metric threshold alerting evaluator (#736): selects merchants with enabled
	// alert rules (indexed) and evaluates each rule under merchant context,
	// edge-triggering threshold crossings and emitting due digests.
	if err := addTrackedWorker(r, workers, &riverjobs.AlertEvalWorker{
		DB:     r.DB,
		Alerts: r.AlertService,
		Clock:  clock,
	}); err != nil {
		return fmt.Errorf("add alert eval worker: %w", err)
	}
	// Low-severity reconciliation-findings digest (#787): a SEPARATE
	// notification source from the rule evaluator above — findings are
	// event-sourced, not a metric-threshold rule.
	if err := addTrackedWorker(r, workers, &riverjobs.FindingsDigestWorker{
		DB:     r.DB,
		Alerts: r.AlertService,
	}); err != nil {
		return fmt.Errorf("add findings digest worker: %w", err)
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
	// #730/#788: every provider intent arms per merchant from the armed rail
	// state at drain time — NMI via the ONE #725 builder, CCBill DataLink and
	// Stripe via the #788 rail resolution seam.
	ccbillCancel := intents.NewCCBillCancelHandler(r.DB, r.Config, r.RailConfigs, clock) // #696 (unarmed rail parks)
	ccbillCancel.DataLinkBaseURL = r.CCBillDataLinkEndpoint
	ccbillRefund := intents.NewCCBillRefundHandler(r.DB, r.Config, r.RailConfigs, clock) // #696 refund (unarmed rail parks)
	ccbillRefund.DataLinkBaseURL = r.CCBillDataLinkEndpoint
	registry := intents.NewRegistry(
		intents.NewNMIDeleteHandler(r.DB, r.Config, r.CollectionResolver, clock),
		intents.NewNMIPaymentSourceUpdateHandler(r.DB, r.CollectionResolver, clock), // #674: payment-method swap
		ccbillCancel,
		intents.NewNMIRefundHandler(r.DB, r.CollectionResolver, clock),
		intents.NewStripeRefundHandler(r.DB, r.Config, r.RailConfigs, clock),
		ccbillRefund,
		intents.NewManualRebillHandler(r.DB, r.Config, r.CollectionResolver, clock),
		intents.NewStripeArchiveProductHandler(r.DB, r.Config, r.RailConfigs, clock),
		intents.NewStripeArchivePriceHandler(r.DB, r.Config, r.RailConfigs, clock),
		intents.NewSolanaSunsetPlanHandler(r.DB, r.SolanaPlanService, r.SolanaRPCResolver.ChainReader(), clock),
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
	registry.Register(intents.NewTopupChargeHandler(r.DB, r.MoneyCharger, r.CollectionResolver, clock))
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
	if r.SolanaRPCResolver != nil {
		// #728: the verify leg reads the chain with the intent's merchant-armed
		// client.
		solanaChain = r.SolanaRPCResolver.ChainReader()
	}
	registry.Register(riverjobs.NewSolanaPullIntentHandler(solanaPullCore, intents.NewStore(r.DB), solanaChain))
	return registry
}

// intentRunner builds a Runner over a registry. Config is attached only when
// non-nil so the origin x mode gate's nil check (= full mode in tests) keeps
// working — a typed-nil ModeView would panic inside the gate.
func (r *Runtime) intentRunner(registry *intents.Registry, clock clockwork.Clock) *intents.Runner {
	runner := &intents.Runner{
		// #732: gate the request-path enqueue chokepoint (vault delete, admin
		// refund) — destructive user/admin ops pass the rate ceiling before the
		// write-ahead intent is created.
		Store:    intents.NewStoreGated(r.DB, r.RateCeiling()),
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

// RateCeiling returns the #732 anti-credential-compromise destructive-op rate
// ceiling, bound to the root pool-backed DB. It is the single gate producers
// wire onto their enqueue chokepoints (schedulers, the intent runner). Cheap to
// build (a thin DB wrapper), so returned fresh per call.
func (r *Runtime) RateCeiling() *intents.RateCeiling {
	return intents.NewRateCeiling(r.DB)
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

	// Every 4 hours: Provider Refresh scheduler (#574/#719) — fans out one
	// per-merchant refresh job (bounded event windows, unknown-cohort
	// reconcile, CCBill DataLink). RunOnStart=true: startup after a stale
	// dump/outage should not wait for the first 4-hour tick; boot enqueues the
	// scheduler which enqueues the merchant jobs.
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

	// Every 5 minutes: flush admission-denial counters (#733) from Redis to PG.
	jobs = append(jobs, r.healthPeriodic(
		5*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.AdmissionDenialFlushArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 5 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Catalog reconciliation loop (issue #209): pull the Stripe catalog and
	// diff it against the OpenRails DB, recording drift + orphan events.
	// Alert-only — never mutates Stripe or the catalog rows. Interval is config
	// catalog_reconciliation_interval (#712; 0 disables, malformed fails here).
	interval, reconcileEnabled, err := r.Config.CatalogReconciliationSchedule()
	if err != nil {
		return nil, err
	}
	if reconcileEnabled {
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

	// Every 15 minutes: metric threshold alerting evaluation (#736). Slow
	// cadence — the metrics it watches (chargeback rate, dunning, depletion) move
	// on hours, not seconds; the monthly digest is cadence-gated inside the rule.
	jobs = append(jobs, r.healthPeriodic(
		15*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.AlertEvalArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 15 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 15 minutes: low-severity reconciliation-findings digest (#787). The
	// outer tick is cheap (indexed armed-merchant scan); the daily cadence gate
	// lives inside DigestFindings, same shape as the payment_methods_expiring
	// digest above.
	jobs = append(jobs, r.healthPeriodic(
		15*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.FindingsDigestArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 15 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
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
