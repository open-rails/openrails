package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/open-rails/openrails/internal/reconcile"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/writeposture"
)

// addBillingWorkersToRegistry adds billing workers to an existing worker registry.
// OpenRails' own River and the host fleet's RiverJobs use this registry.
// merchantRefreshQueue routes the per-merchant refresh jobs: the bounded
// QueueProviderRefresh in standalone, QueueBilling for embedded hosts (whose
// river clients only configure that queue).
func (r *Runtime) addBillingWorkersToRegistry(ctx context.Context, workers *river.Workers, merchantRefreshQueue string) error {
	if err := r.validateBillingWorkerRuntime(); err != nil {
		return err
	}
	// Workers that resolve per-merchant secrets need the merchants service, even
	// on embedded hosts that build no HTTP server. No-op when already set; a
	// failure to arm fails boot.
	if err := r.EnsureMerchantsService(ctx); err != nil {
		return fmt.Errorf("arm merchants service: %w", err)
	}

	clock := r.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	// One registry feeds every worker that runs intents; IntentRunner builds
	// the same set, so per-type semantics never diverge.
	intentRegistry := r.buildIntentRegistry(clock)
	if err := addTrackedWorker(r, workers, &riverjobs.DunningWorker{DB: r.DB, Config: r.Config, Clock: clock, NMIResolver: r.CollectionResolver, EngineCollections: r.MoneyService, DeferDelete: r.DeferredDeletes, Intents: r.intentRunner(intentRegistry, clock)}); err != nil {
		return fmt.Errorf("add dunning worker: %w", err)
	}
	// Provider refresh: the periodic kind is a scheduler that fans out one
	// staggered, per-merchant-unique refresh job, skipping merchants with no PSPs.
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
				Endpoints: reconcile.ProviderEndpoints{CCBillDataLinkBaseURL: config.SandboxCCBillDataLinkURL(r.Config)}},
		}
		r.Verifier.Start()
	}
	// The per-merchant body: bounded provider event pulls, the unknown-cohort
	// reconcile (the one per-subscription verification path), CCBill DataLink,
	// and scoped convergence after refresh writes.
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderRefreshWorker{
		StripeClients: r.StripeClients,
		DB:            r.DB,
		Config:        r.Config,
		Clock:         clock,
		Merchants:     r.Merchants, // per-merchant store-armed pulls
		DeferDelete:   r.DeferredDeletes,
		Contacts:      r.Contacts,
		Alerts:        r.AlertService, // requires_review findings -> operator notifications
		NMIClients:    r.NMIClients,
		PullEndpoints: reconcile.ProviderEndpoints{CCBillDataLinkBaseURL: config.SandboxCCBillDataLinkURL(r.Config)},
		Verifier:      r.Verifier,
		RecoverInvoicePayment: func(ctx context.Context, psp uuid.UUID, transaction string) error {
			mid, err := merchant.Require(ctx)
			if err != nil {
				return err
			}
			receipt, err := money.ReadObservedNMIInvoiceReceipt(ctx, r.CollectionResolver, mid.UUID(), psp, transaction)
			if err != nil {
				return err
			}
			_, err = r.MoneyService.RecoverObservedInvoicePayment(ctx, receipt)
			var owned *money.InvoiceRecoveryOperationOwned
			if errors.As(err, &owned) {
				if wakeErr := intents.NewStore(r.DB).WakeOperation(ctx, owned.OperationID, clock.Now()); wakeErr != nil {
					return wakeErr
				}
			}
			return err
		},
	}); err != nil {
		return fmt.Errorf("add provider refresh worker: %w", err)
	}
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
	// Batch account updater: ingests results for open batches, then opens new
	// ones for instruments renewing inside the custodian's lookahead window.
	// Merchants with no armed custodian are never visited.
	if err := addTrackedWorker(r, workers, &riverjobs.AccountUpdaterBatchWorker{
		DB:        r.DB,
		Config:    r.Config,
		Clock:     clock,
		Rails:     r.RailConfigs,
		Intents:   r.intentRunner(intentRegistry, clock),
		Lifecycle: r.SubscriptionLifecycleService,
	}); err != nil {
		return fmt.Errorf("add account updater worker: %w", err)
	}
	// A decline that says the card was reissued reads its holder once.
	holders, _ := r.CollectionResolver.(paymentmethods.CardHolders)
	if err := addTrackedWorker(r, workers, &riverjobs.CardRefreshWorker{DB: r.DB, Clock: clock, Holders: holders, Lifecycle: r.SubscriptionLifecycleService}); err != nil {
		return fmt.Errorf("add card refresh worker: %w", err)
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
	if err := addTrackedWorker(r, workers, &riverjobs.OrderExpiryWorker{DB: r.DB, Orders: r.Orders, Clock: clock}); err != nil {
		return fmt.Errorf("add order expiry worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.LedgerIntegrityWorker{
		DB:    r.DB,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add ledger integrity worker: %w", err)
	}
	// Reads every currency's FX rates once for the fleet.
	if err := addTrackedWorker(r, workers, &riverjobs.FXRefreshWorker{Rates: r.FXRates}); err != nil {
		return fmt.Errorf("add FX refresh worker: %w", err)
	}
	// Flushes the Redis admission-denial counters to PG hourly aggregates.
	// Redis may be nil (no-admission deployments); the worker no-ops then.
	if err := addTrackedWorker(r, workers, &riverjobs.AdmissionDenialFlushWorker{
		DB:    r.DB,
		Redis: r.RedisClient,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add admission denial flush worker: %w", err)
	}
	// Converge sweep: reconcile.Converge for every active merchant, catching
	// drift no inline mutation touched.
	if err := addTrackedWorker(r, workers, &riverjobs.ConvergeSweepWorker{
		DB:     r.DB,
		Config: r.Config,
		Clock:  clock,
		Alerts: r.AlertService, // requires_review findings -> operator notifications
	}); err != nil {
		return fmt.Errorf("add converge sweep worker: %w", err)
	}
	// Notification email sweep: delivers undelivered notifications rows
	// (emailed_at NULL), including the converge pass's access-ended rows,
	// which are created without inline delivery.
	if err := addTrackedWorker(r, workers, &riverjobs.NotificationEmailSweepWorker{
		DB:            r.DB,
		Notifications: r.NotificationService,
	}); err != nil {
		return fmt.Errorf("add notification email sweep worker: %w", err)
	}
	if err := addTrackedWorker(r, workers, &riverjobs.NotificationEmailWorker{
		DB:            r.DB,
		Notifications: r.NotificationService,
	}); err != nil {
		return fmt.Errorf("add notification email worker: %w", err)
	}
	// Price-migration re-driver: retries failed provider pushes on time. A
	// nil service (worker-only runtimes) skips inside the worker.
	if err := addTrackedWorker(r, workers, &riverjobs.PriceMigrationRedriveWorker{
		Migrations: r.PriceMigrationService,
	}); err != nil {
		return fmt.Errorf("add price migration redrive worker: %w", err)
	}
	// Accepted operations carry their own durable River lifecycle job.
	if err := addTrackedWorker(r, workers, &riverjobs.ProviderOperationWorker{
		DB: r.DB, Config: r.Config, Clock: clock, Registry: intentRegistry,
	}); err != nil {
		return fmt.Errorf("add provider operation worker: %w", err)
	}
	// Webhook wake-ups: the coalesced per-subscription fetch-and-converge job
	// the Stripe/NMI subscription-state handlers enqueue.
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
		RailCustomerService:          r.RailCustomerService,
		CheckoutAttemptService:       r.CheckoutAttemptService,
	}); err != nil {
		return fmt.Errorf("add subscription converge worker: %w", err)
	}
	// NMI attempts are filled from the Query API's transaction report.
	if err := addTrackedWorker(r, workers, &riverjobs.AttemptEnrichmentWorker{
		DB: r.DB, Clock: clock, NMIResolver: r.CollectionResolver,
	}); err != nil {
		return fmt.Errorf("add attempt enrichment worker: %w", err)
	}
	// Each NMI PSP's own history, as monthly aggregates.
	if err := addTrackedWorker(r, workers, &riverjobs.NMIHistoryWorker{
		DB: r.DB, Clock: clock, NMIResolver: r.CollectionResolver,
	}); err != nil {
		return fmt.Errorf("add nmi history worker: %w", err)
	}
	// Rebills that never happened are recorded as missed.
	if err := addTrackedWorker(r, workers, &riverjobs.RebillWatchWorker{
		DB: r.DB, Config: r.Config, Clock: clock, NMIResolver: r.CollectionResolver, Lifecycle: r.SubscriptionLifecycleService,
	}); err != nil {
		return fmt.Errorf("add rebill watch worker: %w", err)
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
	// Solana recurring cranker. Without a Cranker (SetSolanaCranker) it logs
	// and skips.
	solanaCrankWorker := &riverjobs.SolanaCrankWorker{
		DB:        r.DB,
		Config:    r.Config,
		Clock:     clock,
		Lifecycle: r.SubscriptionLifecycleService,
		// Pulls run as durable solana_pull intents through the shared registry.
		Intents: r.intentRunner(intentRegistry, clock),
	}
	if r.SolanaCranker != nil {
		solanaCrankWorker.Cranker = r.SolanaCranker
	}
	if err := addTrackedWorker(r, workers, solanaCrankWorker); err != nil {
		return fmt.Errorf("add solana cranker worker: %w", err)
	}
	// Warns when a merchant's Solana cranker wallet is low on SOL. Alert-only,
	// no auto-top-up.
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
	// Solana ledger reconciliation: cross-checks confirmed on-chain pulls
	// against billing.payments and raises operator repair alerts on drift.
	if err := addTrackedWorker(r, workers, &riverjobs.SolanaReconcileWorker{
		DB:    r.DB,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add solana reconcile worker: %w", err)
	}
	return nil
}

// buildIntentRegistry registers the handler of every provider intent type.
func (r *Runtime) buildIntentRegistry(clock clockwork.Clock) *intents.Registry {
	// Every provider intent arms per merchant at drain time: NMI through
	// CollectionResolver, CCBill and Stripe through RailConfigs.
	ccbillCancel := intents.NewCCBillCancelHandler(r.DB, r.Config, r.RailConfigs, clock) // an unarmed rail parks
	ccbillCancel.DataLinkBaseURL = config.SandboxCCBillDataLinkURL(r.Config)
	ccbillRefund := intents.NewCCBillRefundHandler(r.DB, clock) // retain unresolved pre-qualification refunds
	rebill := intents.NewManualRebillHandler(r.DB, r.Config, r.CollectionResolver, clock)
	rebill.DeferDelete = newProviderCancelScheduler(r.DB, r.RateCeiling(), intents.OriginSystem, "terminal recurring recovery")
	registry := intents.NewRegistry(
		intents.NewNMIDeleteHandler(r.DB, r.Config, r.CollectionResolver, clock),
		intents.NewNMIPaymentSourceUpdateHandler(r.DB, r.CollectionResolver, clock), // payment-method swap
		ccbillCancel,
		intents.NewNMIRefundHandler(r.DB, r.CollectionResolver, clock),
		intents.NewStripeRefundHandler(r.DB, r.Config, r.RailConfigs, clock, r.StripeClients),
		intents.NewStripeCancelHandler(r.DB, r.Config, r.RailConfigs, r.StripeClients, clock),
		ccbillRefund,
		rebill,
		// Invoice collection rides the intent ledger like every other money
		// mover.
		money.NewInvoiceCollectionHandler(r.DB, r.MoneyCharger, r.CollectionResolver, r.Posture(), clock),
		money.NewSubscriptionCollectionHandler(r.DB, r.CollectionResolver, r.Config, clock),
		// The account-updater batch submit is a paid provider write, so it
		// rides the intent ledger too.
		intents.NewAccountUpdaterBatchHandler(r.DB, r.Config, r.RailConfigs, clock),
	)
	// Write-through kinds live with their domain services and register only
	// when those are wired (worker-only runtimes may lack them).
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
	// Durable user-initiated vault writes; a handler with no rail client parks
	// rather than fails.
	if r.RailPaymentMethodService != nil {
		registry.Register(intents.NewNMIPaymentMethodDeleteHandler(r.DB, r.RailPaymentMethodService, r.Clock))
		registry.Register(intents.NewHyperSwitchMethodDeleteHandler(r.DB, r.RailPaymentMethodService, clock))
		registry.Register(intents.NewNMIPaymentMethodUpdateHandler(r.DB, r.RailPaymentMethodService, intents.NewStore(r.DB), clock))
		registry.Register(intents.NewNMICardVaultHandler(r.DB, r.RailPaymentMethodService, intents.NewStore(r.DB), clock))
	}
	// Solana recurring pull: the handler wraps the crank with a pre-submit
	// signature write-ahead and chain-read verification. Its core worker has no
	// Intents runner: the handler is the execution path, and a runner would recurse.
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
		// The verify leg reads the chain with the intent's merchant-armed client.
		solanaChain = r.SolanaRPCResolver.ChainReader()
	}
	registry.Register(riverjobs.NewSolanaPullIntentHandler(solanaPullCore, intents.NewStore(r.DB), solanaChain))
	return registry
}

// intentRunner builds a Runner over a registry. Its gate reads each merchant's
// write posture; a nil Config reads as readonly and parks everything.
func (r *Runtime) intentRunner(registry *intents.Registry, clock clockwork.Clock) *intents.Runner {
	runner := &intents.Runner{
		// Destructive user/admin enqueues pass the rate ceiling before the
		// write-ahead intent is created.
		Store:    intents.NewStoreGated(r.DB, r.RateCeiling()),
		Registry: registry,
		Breaker:  intents.NewVolumeBreaker(r.DB), // gates destructive types everywhere
		// Operator kill switch: one UPDATE halts every destructive provider
		// write on every node.
		Destructive: destructive.New(r.DB),
		Clock:       clock,
		Config:      r.Posture(),
	}
	return runner
}

// Posture reads merchants' provider write postures.
func (r *Runtime) Posture() writeposture.View {
	return writeposture.View{Config: r.Config, DB: r.DB}
}

// IntentRunner returns a Runner for synchronous enqueue+execute from request
// paths, over a fresh registry built like the workers'.
func (r *Runtime) IntentRunner() *intents.Runner {
	clock := r.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	return r.intentRunner(r.buildIntentRegistry(clock), clock)
}

// RateCeiling returns the destructive-op rate ceiling that bounds a compromised
// credential, bound to the root pool. Producers wire it onto their enqueue
// chokepoints; it is a thin DB wrapper, built fresh per call.
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

	// Every 15 minutes: overdue rebills with no attempt are probed (NMI) or
	// recorded as missed.
	jobs = append(jobs, r.healthPeriodic(
		riverjobs.RebillWatchInterval,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.RebillWatchArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: riverjobs.RebillWatchInterval},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Hourly: NMI attempts are enriched from the transaction report.
	jobs = append(jobs, r.healthPeriodic(
		riverjobs.AttemptEnrichmentInterval,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.AttemptEnrichmentArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: riverjobs.AttemptEnrichmentInterval},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Hourly: read the history of NMI PSPs due their daily read.
	jobs = append(jobs, r.healthPeriodic(
		riverjobs.NMIHistoryInterval,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.NMIHistoryArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: riverjobs.NMIHistoryInterval},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every providerrecovery.RefreshInterval: the provider refresh scheduler
	// fans out one job per merchant. Runs on start so recovery after an outage
	// does not wait for the first tick.
	jobs = append(jobs, r.healthPeriodic(
		providerrecovery.RefreshInterval,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.ProviderRefreshArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: providerrecovery.RefreshInterval, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

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

	// Hourly: price-migration re-drive. Period granularity is days, so
	// hourly can never miss a subscription's final pre-effective period.
	// RunOnStart=true: a reboot after downtime is exactly when deferred rows
	// have accumulated, and an empty pass is a cheap indexed no-op.
	jobs = append(jobs, r.healthPeriodic(
		time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.PriceMigrationRedriveArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Every 6 hours: batch account updater. Its window is weeks wide, so 6h
	// gives an in-flight batch four ingest chances a day. Runs on start: a batch
	// submitted before a crash is polled, never resubmitted (its row holds the
	// job ref; one open batch per custodian).
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

	// Every hour: retention cleanup of expired and aged rows.
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

	// Every 15 minutes: delete expired request and webhook claims.
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

	// Every hour: crank due Solana recurring subscriptions. The next_pull_at
	// due-query, not the tick, follows the billing cadence.
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

	// Every 6 hours: alert on low Solana cranker-wallet SOL gas.
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

	// Every 6 hours: reconcile confirmed Solana pulls against the ledger.
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

	// Every 15 minutes: delete settled Solana Pay references.
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

	// Every 5 minutes: expire orders past their expiry.
	jobs = append(jobs, r.healthPeriodic(
		5*time.Minute,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.OrderExpiryArgs{}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 5 * time.Minute},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 2 hours: one FX refresh for the fleet (34 requests), which every
	// replica quotes from. Not on start: the stored rates outlive a restart,
	// and a quote that finds none fresh reads its base currency itself.
	jobs = append(jobs, r.healthPeriodic(
		fx.RefreshInterval,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.FXRefreshArgs{}, riverjobs.FXRefreshInsertOpts()
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	))

	// Every 5 minutes: flush admission-denial counters from Redis to PG.
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

	// Catalog reconciliation: pull the Stripe and NMI catalogs and record drift
	// against the DB. Alert-only, never mutates providers or catalog rows.
	// Cadence is catalog_reconciliation_interval (0 disables, malformed fails here).
	interval, reconcileEnabled, err := config.CatalogReconciliationSchedule(r.Config)
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

	// Invoice jobs run on start to recover overdue work; period uniqueness keeps
	// each cadence across replicas and restarts.
	// Every hour: invoice collection.
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
		&river.PeriodicJobOpts{RunOnStart: true},
	))
	// Monthly invoice sweep: collect the long tail above the merchant's floor
	// that the hourly threshold trigger leaves behind.
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
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Daily: finalize each customer's previous invoice period. Idempotent per
	// (customer, currency, period).
	jobs = append(jobs, r.healthPeriodic(
		24*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) {
			return riverjobs.InvoiceArgs{FinalizePreviousMonth: true}, &river.InsertOpts{
				Queue:      riverjobs.QueueBilling,
				UniqueOpts: river.UniqueOpts{ByQueue: true, ByPeriod: 24 * time.Hour},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// Every 15 minutes: arrears delinquency evaluation. Tighter than hourly
	// collection on purpose: the exit half stops telling a customer who has
	// already paid that they cannot spend.
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

	// Every 15 minutes: converge sweep, remediating internal drift (stalled
	// dunning, elapsed grace, abandoned checkouts, unmaterialized grant effects).
	// Runs on start: drift is largest after downtime, and a clean sweep is cheap.
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

	// Every 10 minutes: retry undelivered notification emails (emailed_at NULL).
	// Cheap no-op when nothing is queued or no email service is armed.
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

	// Daily: ledger integrity audit. The account counters are a trigger-
	// maintained projection, broken only by writes that bypass the trigger
	// (restore, COPY, triggers off), which leave nothing to react to; daily
	// bounds how long a wrong balance compounds unnoticed.
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

	// No worker-health job: the progress detector runs outside River
	// (StartRiverProgressMonitor), so a stalled River cannot silence it.

	return jobs, nil
}
