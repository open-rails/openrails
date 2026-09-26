package app

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/reconcile"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// addBillingWorkersToRegistry adds billing workers to an existing worker registry.
// Both managed and host-owned RiverJobs contributions use this registry.
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
	if err := addTrackedWorker(r, workers, &riverjobs.DunningWorker{DB: r.DB, Config: r.Config, Clock: clock, NMIResolver: r.CollectionResolver, EngineCollections: r.MoneyService, DeferDelete: r.DeferredDeletes, Intents: r.intentRunner(intentRegistry, clock)}); err != nil {
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
	if r.Verifier == nil {
		r.Verifier = &reconcile.Verifier{
			DB: r.DB, Clock: clock, DeferDelete: r.DeferredDeletes, Notifications: r.NotificationService,
			Builder: reconcile.MerchantFetcherBuilder{StripeClients: r.StripeClients, Config: r.Config, Merchants: r.Merchants, DB: r.DB, NMIClients: r.NMIClients,
				Endpoints: reconcile.ProviderEndpoints{CCBillDataLinkBaseURL: r.Config.SandboxCCBillDataLinkURL()}},
		}
		r.Verifier.Start()
	}
	// The per-merchant body: bounded provider event pulls, the unknown-cohort
	// reconcile (#632/#665 — the one per-subscription verification path; the
	// #367 liveness worker is retired), CCBill DataLink, and scoped
	// convergence after refresh writes.
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderRefreshWorker{
		StripeClients:       r.StripeClients,
		DB:                  r.DB,
		Config:              r.Config,
		Clock:               clock,
		Merchants:           r.Merchants, // #699/#788: per-merchant store-armed pulls
		DeferDelete:         r.DeferredDeletes,
		NotificationService: r.NotificationService,
		Alerts:              r.AlertService, // #787: requires_review findings -> operator notifications
		NMIClients:          r.NMIClients,
		PullEndpoints:       reconcile.ProviderEndpoints{CCBillDataLinkBaseURL: r.Config.SandboxCCBillDataLinkURL()},
		Verifier:            r.Verifier,
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
	if err := addTrackedWorker(r, workers, &riverjobs.IdempotencyGCWorker{DB: r.DB}); err != nil {
		return fmt.Errorf("add idempotency gc worker: %w", err)
	}
	// or#795: the batch account-updater cadence. Ingests results for open
	// batches, then opens new ones for instruments renewing inside the
	// custodian's lookahead window. Merchants with no armed custodian are never
	// visited (the work queue starts at the custodian registry).
	if err := addTrackedWorker(r, workers, &riverjobs.AccountUpdaterBatchWorker{
		DB:      r.DB,
		Config:  r.Config,
		Clock:   clock,
		Rails:   r.RailConfigs,
		Intents: r.intentRunner(intentRegistry, clock),
	}); err != nil {
		return fmt.Errorf("add account updater worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.SolanaPayGCWorker{DB: r.DB, Clock: clock}); err != nil {
		return fmt.Errorf("add solana pay gc worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.CreditExpiryWorker{
		DB:    r.DB,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add credit expiry worker: %w", err)
	}
	// or#833: the ledger integrity checks existed but nothing ran them.
	if err := addTrackedWorker(r, workers, &riverjobs.LedgerIntegrityWorker{
		DB:    r.DB,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add ledger integrity worker: %w", err)
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
	// Notification email sweep (#789): delivers undelivered notifications
	// rows (emailed_at NULL) — including the converge NOTIFY pass's access-ended
	// rows, which are created without inline delivery.
	if err := addTrackedWorker(r, workers, &riverjobs.NotificationEmailSweepWorker{
		DB:            r.DB,
		Notifications: r.NotificationService,
	}); err != nil {
		return fmt.Errorf("add notification email sweep worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.CancelSubscriptionWorker{
		StripeClients:                r.StripeClients,
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
		StripeClients:                r.StripeClients,
		Clock:                        r.Clock,
		DB:                           r.DB,
		Config:                       r.Config,
		Rails:                        r.RailConfigs,
		EntitlementService:           r.EntitlementService,
		SubscriptionService:          r.SubscriptionService,
		SubscriptionLifecycleService: r.SubscriptionLifecycleService,
	}); err != nil {
		return fmt.Errorf("add resume subscription worker: %w", err)
	}
	// Plan-migration re-driver (#816): re-drives blocked #813 plan-change rows
	// (deferred far-future pushes entering their final pre-effective period;
	// crash-window rows whose rail already carries the target) through the same
	// idempotent execute paths a manual re-run uses. Nil service (worker-only
	// runtimes) log-and-skips inside the worker.
	if err := addTrackedWorker(r, workers, &riverjobs.PlanMigrationRedriveWorker{
		Migrations: r.PlanMigrationService,
	}); err != nil {
		return fmt.Errorf("add plan migration redrive worker: %w", err)
	}
	// Accepted operations carry their own durable River lifecycle job.
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderOperationWorker{
		DB: r.DB, Config: r.Config, Clock: clock, Registry: intentRegistry,
	}); err != nil {
		return fmt.Errorf("add provider operation worker: %w", err)
	}
	// #684: webhook wake-ups — the coalesced per-subscription fetch-and-converge
	// job the slimmed Stripe/NMI subscription-state handlers enqueue.
	if err := addTrackedWorker(r, workers, &riverjobs.SubscriptionConvergeWorker{
		StripeClients:                r.StripeClients,
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
		StripeClients: r.StripeClients,
		DB:            r.DB,
		Config:        r.Config,
		Rails:         r.RailConfigs,
		NMIResolver:   r.CollectionResolver,
	}); err != nil {
		return fmt.Errorf("add catalog reconciliation worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.StripeWebhookReconcileWorker{
		StripeClients: r.StripeClients,
		DB:            r.DB,
		Config:        r.Config,
		Merchants:     r.Merchants,
	}); err != nil {
		return fmt.Errorf("add stripe webhook reconcile worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.MerchantSecretCleanupWorker{DB: r.DB, Merchants: r.Merchants}); err != nil {
		return fmt.Errorf("add merchant secret cleanup worker: %w", err)
	}
	// Invoice collection and reconciliation workers. Collection waits until
	// an off-session charger is configured.
	if err := addTrackedWorker(r, workers, &riverjobs.InvoiceWorker{
		DB:      r.DB,
		Money:   r.MoneyService,
		Intents: r.intentRunner(intentRegistry, clock),
		Config:  r.Config,
		Clock:   clock,
	}); err != nil {
		return fmt.Errorf("add invoice worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.DelinquencyWorker{
		DB:    r.DB,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add delinquency worker: %w", err)
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
	// Jobs orphaned by a dead process (their liveness beat stopped) return to
	// work; River's rescuer skips OpenRails' timeout-free jobs.
	if err := addTrackedWorker(r, workers, &riverjobs.JobRescueWorker{River: r.riverTableAccess}); err != nil {
		return fmt.Errorf("add job rescue worker: %w", err)
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
	// #895: there is deliberately NO worker-health-check WORKER here any more.
	// The detector used to be a River periodic job, so a stalled River stalled
	// its own detector and could only report health where health was never in
	// doubt. It now lives outside River entirely, as riverjobs.ProgressMonitor —
	// a plain goroutine started by Runtime.StartRiverProgressMonitor.
	return nil
}

// buildIntentRegistry assembles the per-type intent semantics for the
// provider intent executor/verifier (#358): deferred NMI deletes (phase A),
// NMI/Stripe refunds (phase B), manual rebills (phase C), catalog archive
// ops — Stripe product/price archives + Solana plan sunsets (phase D) — and
// the #674 write-through kinds (checkout NMI sales,
// Solana recurring pulls).
func (r *Runtime) buildIntentRegistry(clock clockwork.Clock) *intents.Registry {
	// #730/#788: every provider intent arms per merchant from the armed rail
	// state at drain time — NMI via the ONE #725 builder, CCBill DataLink and
	// Stripe via the #788 rail resolution seam.
	ccbillCancel := intents.NewCCBillCancelHandler(r.DB, r.Config, r.RailConfigs, clock) // #696 (unarmed rail parks)
	ccbillCancel.DataLinkBaseURL = r.Config.SandboxCCBillDataLinkURL()
	ccbillRefund := intents.NewCCBillRefundHandler(r.DB, clock) // retain unresolved pre-qualification refunds
	rebill := intents.NewManualRebillHandler(r.DB, r.Config, r.CollectionResolver, clock)
	rebill.DeferDelete = newProviderCancelScheduler(r.DB, r.RateCeiling(), intents.OriginSystem, "terminal recurring recovery")
	registry := intents.NewRegistry(
		intents.NewNMIDeleteHandler(r.DB, r.Config, r.CollectionResolver, clock),
		&intents.NMIProviderCutover{DB: r.DB, Resolver: r.CollectionResolver, Clock: clock},
		&intents.NMIEngineTakeover{DB: r.DB, Resolver: r.CollectionResolver, Clock: clock},
		intents.NewNMIPaymentSourceUpdateHandler(r.DB, r.CollectionResolver, clock), // #674: payment-method swap
		ccbillCancel,
		intents.NewNMIRefundHandler(r.DB, r.CollectionResolver, clock),
		intents.NewStripeRefundHandler(r.DB, r.Config, r.RailConfigs, clock, r.StripeClients),
		intents.NewStripeCancelHandler(r.DB, r.Config, r.RailConfigs, r.StripeClients),
		ccbillRefund,
		rebill,
		// Invoice collection rides the ledger like every other money mover; the
		// charger and reconciliation reads are the #725 store-armed plane.
		money.NewInvoiceCollectionHandler(r.DB, r.MoneyCharger, r.CollectionResolver, r.Config, clock),
		money.NewSubscriptionCollectionHandler(r.DB, r.CollectionResolver, r.Config, clock),
		intents.NewStripeArchiveProductHandler(r.DB, r.Config, r.RailConfigs, clock, r.StripeClients),
		intents.NewStripeArchivePriceHandler(r.DB, r.Config, r.RailConfigs, clock, r.StripeClients),
		intents.NewSolanaSunsetPlanHandler(r.DB, r.SolanaPlanService, r.SolanaRPCResolver.ChainReader(), clock),
		// or#795: the batch account-updater submit (create job + upload the
		// token CSV) is a paid provider write, so it rides the ledger like
		// every other outbound mutation.
		intents.NewAccountUpdaterBatchHandler(r.DB, r.Config, r.RailConfigs, clock),
	)
	// #674 write-through kinds live next to their domain services and register
	// only when those services are wired (worker-only runtimes may lack them).
	if r.CheckoutService != nil {
		if r.CheckoutService.NMISaleService != nil {
			registry.Register(checkout.NewNMISaleIntentHandler(r.CheckoutService.NMISaleService))
		}
		if r.CheckoutService.CustodianSaleService != nil {
			registry.Register(checkout.NewCustodianSaleIntentHandler(r.CheckoutService.CustodianSaleService))
		}
		registry.Register(checkout.NewInitialMembershipIntentHandler(r.CheckoutService, r.CollectionResolver))
		registry.Register(checkout.NewNMIUpgradeIntentHandler(r.CheckoutService))
		registry.Register(checkout.NewStripeTierChangeIntentHandler(r.CheckoutService))
	}
	// #674 tail: durable user-initiated vault deletes (an unwired RailPaymentMethodService
	// resolves no client, so the handler parks — never fails).
	if r.RailPaymentMethodService != nil {
		registry.Register(intents.NewNMIPaymentMethodDeleteHandler(r.DB, r.RailPaymentMethodService, r.Clock))
		registry.Register(intents.NewHyperSwitchMethodDeleteHandler(r.DB, r.RailPaymentMethodService, clock))
		registry.Register(intents.NewNMIPaymentMethodUpdateHandler(r.DB, r.RailPaymentMethodService, intents.NewStore(r.DB), clock))
	}
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
// non-nil: since or#865 a nil ModeView fails CLOSED (everything parks), so
// handing the gate a typed-nil interface would silently park production work.
// It does NOT panic — (*config.Config).normalizedProviderWriteMode nil-guards
// its receiver and a typed nil reads as readonly, which parks just the same.
func (r *Runtime) intentRunner(registry *intents.Registry, clock clockwork.Clock) *intents.Runner {
	runner := &intents.Runner{
		// #732: gate the request-path enqueue chokepoint (vault delete, admin
		// refund) — destructive user/admin ops pass the rate ceiling before the
		// write-ahead intent is created.
		Store:    intents.NewStoreGated(r.DB, r.RateCeiling()),
		Registry: registry,
		Breaker:  intents.NewVolumeBreaker(r.DB), // #679: gate destructive types everywhere
		// #836: the operator kill switch — one UPDATE halts every destructive
		// provider write on every node, no deploy.
		Destructive: destructive.New(r.DB),
		Clock:       clock,
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

	// The due pass admits engine renewals at their paid-period boundary and
	// runs retries whose next_retry_at has passed. Engine access is bounded by
	// the paid period, so the pass cadence is the longest a paying member can
	// wait at the boundary; it runs on start so a restart never adds a lag.
	// An idle pass is one indexed work-queue read.
	jobs = append(jobs, r.healthPeriodic(
		riverjobs.DuePassInterval,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.DunningArgs{}, &river.InsertOpts{
				Queue: riverjobs.QueueBilling,
				// Coalesce outstanding scans, but let startup run again after
				// a completed scan. A minute bucket including completed jobs
				// would silently suppress RunOnStart after a quick restart.
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByState: []rivertype.JobState{
					rivertype.JobStateAvailable, rivertype.JobStatePending,
					rivertype.JobStateRunning, rivertype.JobStateRetryable,
					rivertype.JobStateScheduled,
				}},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
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

	// Every minute and on start: return OpenRails jobs whose process died.
	jobs = append(jobs, r.healthPeriodic(
		time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.JobRescueArgs{}, &river.InsertOpts{
				Queue: riverjobs.QueueBilling,
				// Completed rescues must not suppress quick restarts. Bound
				// active uniqueness by period too: if this rescuer dies while
				// running, a later pass must be able to rescue it. River's
				// built-in rescuer ignores our negative-timeout workers.
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Minute, ByState: []rivertype.JobState{
					rivertype.JobStateAvailable, rivertype.JobStatePending,
					rivertype.JobStateRunning, rivertype.JobStateRetryable,
					rivertype.JobStateScheduled,
				}},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Hourly: plan-migration re-drive (#816). Period granularity is days, so
	// hourly can never miss a subscription's final pre-effective period.
	// RunOnStart=true: a reboot after downtime is exactly when deferred rows
	// have accumulated, and an empty pass is a cheap indexed no-op.
	jobs = append(jobs, r.healthPeriodic(
		time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.PlanMigrationRedriveArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Every 6 hours: the batch account-updater cadence (or#795). The window it
	// works against is weeks wide, so the tick only has to be much finer than
	// that — 6h gives an in-flight batch four chances a day to be ingested.
	// RunOnStart=true: a restart is exactly when a batch submitted before the
	// crash is waiting to be POLLED (never resubmitted — the durable batch row
	// holds the job ref, and the DB allows one open batch per custodian).
	jobs = append(jobs, r.healthPeriodic(
		6*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.AccountUpdaterBatchArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 6 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
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

	// Every 15 minutes: delete expired request and webhook claims (#1099).
	jobs = append(jobs, r.healthPeriodic(
		15*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.IdempotencyGCArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 15 * time.Minute},
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

	// Every 15 minutes: delete settled Solana Pay references (#1086).
	jobs = append(jobs, r.healthPeriodic(
		15*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.SolanaPayGCArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 15 * time.Minute},
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

	jobs = append(jobs, r.healthPeriodic(5*time.Minute, func() (river.JobArgs, *river.InsertOpts) {
		return riverjobs.MerchantSecretCleanupArgs{}, &river.InsertOpts{Queue: riverjobs.QueueBilling, UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 5 * time.Minute}}
	}, &river.PeriodicJobOpts{RunOnStart: true}))

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

	// Every 15 minutes: arrears delinquency evaluation (or#878). Tighter than
	// the hourly collection pass on purpose — the exit half of this state
	// machine is what stops telling a customer who has already paid that they
	// cannot spend.
	jobs = append(jobs, r.healthPeriodic(
		15*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.DelinquencyArgs{}, &river.InsertOpts{
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

	// Every 10 minutes: notification email sweep (#789) — retry undelivered
	// notification emails (emailed_at NULL). Cheap no-op when nothing is queued
	// or no email service is armed.
	jobs = append(jobs, r.healthPeriodic(
		10*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.NotificationEmailSweepArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 10 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Daily: ledger integrity audit (or#833). The account counters are a
	// trigger-maintained projection, so the only thing that can break them is a
	// write that bypassed the trigger (restore, COPY, a migration with triggers
	// off) — no event, no watermark, no error. Nothing can be pushed, so a slow
	// periodic look is the only detector; daily bounds how long a wrong balance
	// can compound before an operator hears about it.
	jobs = append(jobs, r.healthPeriodic(
		24*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.LedgerIntegrityArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 24 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// #895: the worker-health check is NOT scheduled here any more. Scheduling
	// the detector as a periodic job is what made "River is not running"
	// undetectable — the detector could not run either. riverjobs.ProgressMonitor
	// replaces it, outside River.

	return jobs, nil
}
