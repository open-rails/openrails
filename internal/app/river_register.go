package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/pull"
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
	if err := addWorker(workers, &riverjobs.DestructiveAlertWorker{DB: r.DB, Alerts: r.AlertService}); err != nil {
		return fmt.Errorf("add destructive alert worker: %w", err)
	}
	intentRegistry := r.buildIntentRegistry(clock)
	if err := addWorker(workers, &riverjobs.DunningWorker{DB: r.DB, Config: r.Config, Clock: clock, NMIResolver: r.CollectionResolver, EngineCollections: r.MoneyService}); err != nil {
		return fmt.Errorf("add dunning worker: %w", err)
	}
	// Provider refresh: the periodic kind is a scheduler that fans out one
	// staggered, per-merchant-unique refresh job, skipping merchants with no PSPs.
	if err := addWorker(workers, &riverjobs.ProviderRefreshSchedulerWorker{
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
	if err := addWorker(workers, &riverjobs.ProviderRefreshWorker{
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
		RecoverUsagePayment: func(ctx context.Context, psp uuid.UUID, transaction string) error {
			mid, err := merchant.Require(ctx)
			if err != nil {
				return err
			}
			receipt, err := money.ReadObservedNMIUsageReceipt(ctx, r.CollectionResolver, mid.UUID(), psp, transaction)
			if err != nil {
				return err
			}
			_, err = r.MoneyService.RecoverObservedUsagePayment(ctx, receipt)
			var owned *money.UsageRecoveryOperationOwned
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
	cleanupConfig := riverjobs.DefaultCleanupConfig()
	eventRetention, err := config.EventRetentionDuration(r.Config)
	if err != nil {
		return err
	}
	cleanupConfig.EventRetention = eventRetention
	if err := addWorker(workers, &riverjobs.CleanupExpiredDataWorker{
		DB:     r.DB,
		Clock:  clock,
		Config: cleanupConfig,
	}); err != nil {
		return fmt.Errorf("add cleanup expired data worker: %w", err)
	}
	// Stored cards a vault must be asked about: one refresh per card, queued on
	// an expired or reissued decline and ahead of each renewal.
	updaters, _ := r.CollectionResolver.(paymentmethods.AccountUpdaters)
	if err := addWorker(workers, &riverjobs.StoredCardRefreshWorker{DB: r.DB, Clock: clock, Updaters: updaters, Intents: r.intentRunner(intentRegistry, clock), Lifecycle: r.SubscriptionLifecycleService}); err != nil {
		return fmt.Errorf("add stored card refresh worker: %w", err)
	}
	if err := addWorker(workers, &riverjobs.StoredCardRefreshDueWorker{DB: r.DB, Clock: clock, Merchants: func() *merchants.Service { return r.Merchants }, Posture: r.Posture()}); err != nil {
		return fmt.Errorf("add stored card refresh pass worker: %w", err)
	}
	if err := addWorker(workers, &riverjobs.BalanceWatchWorker{DB: r.DB, Money: r.MoneyService}); err != nil {
		return fmt.Errorf("add balance watch worker: %w", err)
	}
	if err := addWorker(workers, &riverjobs.OrderExpiryWorker{DB: r.DB, Orders: r.Orders, Clock: clock}); err != nil {
		return fmt.Errorf("add order expiry worker: %w", err)
	}
	// Flushes closed hours of the Redis admission-denial counters to PG.
	// Redis may be nil (no-admission deployments); the worker no-ops then.
	if err := addWorker(workers, &riverjobs.AdmissionDenialFlushWorker{
		DB:    r.DB,
		Redis: r.RedisClient,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add admission denial flush worker: %w", err)
	}
	// Converge sweep: reconcile.Converge for every active merchant, catching
	// drift no inline mutation touched.
	if err := addWorker(workers, &riverjobs.ConvergeSweepWorker{
		DB:     r.DB,
		Config: r.Config,
		Clock:  clock,
		Alerts: r.AlertService, // requires_review findings -> operator notifications
	}); err != nil {
		return fmt.Errorf("add converge sweep worker: %w", err)
	}
	// Accepted operations carry their own durable River lifecycle job.
	if err := addWorker(workers, &riverjobs.ProviderOperationWorker{
		DB: r.DB, Config: r.Config, Clock: clock, Runner: r.intentRunner(intentRegistry, clock),
	}); err != nil {
		return fmt.Errorf("add provider operation worker: %w", err)
	}
	// Webhook wake-ups: the coalesced per-subscription fetch-and-converge job
	// the Stripe/NMI subscription-state handlers enqueue.
	if err := addWorker(workers, &riverjobs.SubscriptionConvergeWorker{
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
		OrderAttemptService:          r.OrderAttemptService,
		Objects:                      r.ProviderObjects,
		Importer:                     r.PSPImporter,
		Findings:                     r.findingNotifier(),
	}); err != nil {
		return fmt.Errorf("add subscription converge worker: %w", err)
	}
	// Each NMI PSP's own history, as monthly aggregates.
	if err := addWorker(workers, &riverjobs.NMIHistoryWorker{
		DB: r.DB, Clock: clock, NMIResolver: r.CollectionResolver,
	}); err != nil {
		return fmt.Errorf("add nmi history worker: %w", err)
	}
	// Catalog objects a Stripe event announced, read by id.
	if err := addWorker(workers, &riverjobs.ProviderObjectObserveWorker{Objects: r.ProviderObjects}); err != nil {
		return fmt.Errorf("add provider object observe worker: %w", err)
	}
	// PSP imports: a read-only pull of a PSP, then its landing.
	if err := addWorker(workers, &riverjobs.PSPImportWorker{Importer: r.PSPImporter}); err != nil {
		return fmt.Errorf("add psp import worker: %w", err)
	}
	// Declared charges without an amount, read from their provider once.
	if err := addWorker(workers, &riverjobs.ChargeAttributionWorker{
		DB: r.DB, Clock: clock, NMIResolver: r.CollectionResolver, Rails: r.RailConfigs,
	}); err != nil {
		return fmt.Errorf("add charge attribution worker: %w", err)
	}
	// Rebills that never happened are recorded as missed.
	if err := addWorker(workers, &riverjobs.RebillWatchWorker{
		DB: r.DB, Config: r.Config, Clock: clock, NMIResolver: r.CollectionResolver, Lifecycle: r.SubscriptionLifecycleService,
	}); err != nil {
		return fmt.Errorf("add rebill watch worker: %w", err)
	}
	if err := addWorker(workers, &riverjobs.StripeWebhookReconcileWorker{
		StripeClients: r.StripeClients,
		DB:            r.DB,
		Config:        r.Config,
		Merchants:     r.Merchants,
	}); err != nil {
		return fmt.Errorf("add stripe webhook reconcile worker: %w", err)
	}
	// A merchant purge's Vault cleanup, queued with the purge.
	if err := addWorker(workers, &riverjobs.MerchantSecretCleanupWorker{Merchants: r.Merchants}); err != nil {
		return fmt.Errorf("add merchant secret cleanup worker: %w", err)
	}
	// Usage orders: due accounts billed, due collections queued as
	// operations.
	if err := addWorker(workers, &riverjobs.UsageOrderWorker{
		DB:     r.DB,
		Money:  r.MoneyService,
		Config: r.Config,
		Clock:  clock,
	}); err != nil {
		return fmt.Errorf("add usage order worker: %w", err)
	}
	if err := addWorker(workers, &riverjobs.DelinquencyWorker{
		DB:    r.DB,
		Clock: clock,
	}); err != nil {
		return fmt.Errorf("add delinquency worker: %w", err)
	}
	// Solana recurring cranker: only a deployment with a cranker pulls.
	if r.SolanaCranker != nil {
		solanaCrankWorker := &riverjobs.SolanaCrankWorker{
			DB:     r.DB,
			Config: r.Config,
			Clock:  clock,
			// Pulls run as durable solana_pull intents through the shared registry.
			Intents:  r.intentRunner(intentRegistry, clock),
			Balances: r.SolanaCrankerBalances,
		}
		if err := addWorker(workers, solanaCrankWorker); err != nil {
			return fmt.Errorf("add solana cranker worker: %w", err)
		}
	}
	return nil
}

// addWorker registers an OpenRails worker. Its timeout is its kind's budget,
// which job health reads; the client's JobTimeout is the host's.
func addWorker[T river.JobArgs](workers *river.Workers, worker river.Worker[T]) error {
	var args T
	want := riverjobs.Timeout(args.Kind())
	if want == 0 {
		return fmt.Errorf("%s is not an OpenRails job kind", args.Kind())
	}
	if got := worker.Timeout(&river.Job[T]{JobRow: &rivertype.JobRow{Kind: args.Kind()}}); got != want {
		return fmt.Errorf("%s: timeout %s is not its kind's budget %s", args.Kind(), got, want)
	}
	return river.AddWorkerSafely(workers, worker)
}

// buildIntentRegistry registers the handler of every provider intent type.
func (r *Runtime) buildIntentRegistry(clock clockwork.Clock) *intents.Registry {
	// Every provider intent arms per merchant at drain time: NMI through
	// CollectionResolver, CCBill and Stripe through RailConfigs.
	ccbillCancel := intents.NewCCBillCancelHandler(r.DB, r.Config, r.RailConfigs, clock) // an unarmed rail parks
	ccbillCancel.DataLinkBaseURL = config.SandboxCCBillDataLinkURL(r.Config)
	rebill := intents.NewManualRebillHandler(r.DB, r.Config, r.CollectionResolver, clock)
	rebill.DeferDelete = newProviderCancelScheduler(r.DB, r.RateCeiling(), intents.OriginSystem, "terminal recurring recovery")
	registry := intents.NewRegistry(
		intents.NewNMIRecurringConfirmationHandler(r.DB, r.Config, r.CollectionResolver, clock),
		intents.NewNMIDeleteHandler(r.DB, r.Config, r.CollectionResolver, clock),
		intents.NewNMIPaymentSourceUpdateHandler(r.DB, r.CollectionResolver, clock), // payment-method swap
		ccbillCancel,
		intents.NewNMIRefundHandler(r.DB, r.CollectionResolver, clock),
		intents.NewStripeRefundHandler(r.DB, r.Config, r.RailConfigs, clock, r.StripeClients),
		intents.NewStripeCancelHandler(r.DB, r.Config, r.RailConfigs, r.StripeClients, clock),
		intents.NewStripePaymentMethodDomainHandler(r.Config, r.RailConfigs, r.StripeClients),
		rebill,
		// Order collection rides the intent ledger like every other money
		// mover.
		money.NewOrderCollectionHandler(r.DB, r.MoneyCharger, r.CollectionResolver, r.Posture(), clock),
		money.NewSubscriptionCollectionHandler(r.DB, r.CollectionResolver, r.Config, clock),
		// Write-through kinds live with their domain services, which every
		// worker runtime has (validateBillingWorkerRuntime): an accepted
		// operation of an unregistered kind would never run.
		payment.NewNMISaleIntentHandler(r.PurchaseService.NMISaleService),
		payment.NewInitialMembershipIntentHandler(r.PurchaseService, r.CollectionResolver),
		payment.NewNMIUpgradeIntentHandler(r.PurchaseService),
		payment.NewStripeTierChangeIntentHandler(r.PurchaseService),
		// A price migration's moves carried to Stripe and NMI schedules.
		intents.NewPriceMigrationPushHandler(r.DB, r.PriceMigrationService),
		// Durable user-initiated vault writes; a handler with no rail client
		// parks rather than fails.
		intents.NewNMIPaymentMethodDeleteHandler(r.DB, r.RailPaymentMethodService, clock),
		intents.NewHyperSwitchMethodDeleteHandler(r.DB, r.RailPaymentMethodService, clock),
		intents.NewPlatformMethodDeleteHandler(r.DB, r.RailPaymentMethodService, clock),
		intents.NewNMICardVaultHandler(r.DB, r.RailPaymentMethodService, intents.NewStore(r.DB), r.Posture(), clock),
	)
	// An account-updater request is billed and may mint a vault token.
	if updaters, ok := r.CollectionResolver.(paymentmethods.AccountUpdaters); ok {
		registry.Register(&riverjobs.AccountUpdaterHandler{DB: r.DB, Clock: clock, Updaters: updaters, Lifecycle: r.SubscriptionLifecycleService, Policy: intents.DefaultBackoff})
	}
	// Solana recurring pull: signature write-ahead before the send, chain-read
	// verification after it. The verify leg reads the chain with the intent's
	// merchant-armed client.
	solanaPull := pull.Handler{DB: r.DB, Clock: clock, Lifecycle: r.SubscriptionLifecycleService, Store: intents.NewStore(r.DB)}
	if r.SolanaCranker != nil {
		solanaPull.Cranker = r.SolanaCranker
	}
	if r.SolanaRPCResolver != nil {
		solanaPull.Chain = r.SolanaRPCResolver.ChainReader()
	}
	registry.Register(pull.NewHandler(solanaPull))
	return registry
}

// intentRunner builds a Runner over a registry. Its gate reads each merchant's
// write posture; a nil Config reads as readonly and parks everything.
func (r *Runtime) intentRunner(registry *intents.Registry, clock clockwork.Clock) *intents.Runner {
	runner := &intents.Runner{
		CardAbuseGuard:    r.CardAbuseGuard,
		CardFailureLedger: r.CardFailureLedger,
		// Destructive user/admin enqueues pass the rate ceiling before the
		// write-ahead intent is created.
		Store:    intents.NewStoreGated(r.DB, r.RateCeiling()),
		Registry: registry,
		Breaker:  intents.NewVolumeBreaker(r.DB), // gates destructive types everywhere
		// Operator kill switch: one UPDATE halts every destructive provider
		// write on every node.
		Destructive: destructive.New(r.DB),
		Admissions:  r.DestructiveAdmissions,
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
	if r.PurchaseService == nil || r.PurchaseService.NMISaleService == nil || r.PurchaseService.CustodianSaleService == nil {
		return fmt.Errorf("billing worker runtime purchase service is required")
	}
	if r.RailPaymentMethodService == nil {
		return fmt.Errorf("billing worker runtime payment method service is required")
	}
	return nil
}

// buildRiverPeriodicJobs declares the periodic passes whose gate this runtime
// opens.
func (r *Runtime) buildRiverPeriodicJobs(cfg *river.Config) []*river.PeriodicJob {
	var jobs []*river.PeriodicJob
	for _, p := range riverjobs.Periodics {
		if r.declares(p.Gate) {
			jobs = append(jobs, r.periodicJob(cfg, p))
		}
	}
	return jobs
}

func (r *Runtime) declares(gate riverjobs.Gate) bool {
	switch gate {
	case riverjobs.WithUsageBilling:
		return r.Config != nil && r.Config.UsageBillingEnabled
	case riverjobs.WithSolanaCranker:
		return r.SolanaCranker != nil
	}
	return true
}

// findingNotifier is the alert service as a finding notifier; nil when none.
func (r *Runtime) findingNotifier() reconcile.FindingNotifier {
	if r.AlertService == nil {
		return nil
	}
	return r.AlertService
}
