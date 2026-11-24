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
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/money/fees"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/pull"
	"github.com/open-rails/openrails/internal/reconcile"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/writeposture"
)

// addBillingWorkersToRegistry adds billing workers to an existing worker registry.
// OpenRails' own River and the host fleet's RiverJobs use this registry.
func (r *Runtime) addBillingWorkersToRegistry(ctx context.Context, registry *river.Workers) error {
	workers := &workerSet{river: registry}
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
	if err := addWorker(workers, &riverjobs.OperatorAlertWorker{DB: r.DB, Alerts: r.AlertService}); err != nil {
		return fmt.Errorf("add operator alert worker: %w", err)
	}
	intentRegistry := r.buildIntentRegistry(clock)
	if r.Verifier == nil {
		r.Verifier = &reconcile.Verifier{
			DB: r.DB, Clock: clock, DeferDelete: r.DeferredDeletes, Notifications: r.NotificationService,
			Builder: reconcile.MerchantFetcherBuilder{StripeClients: r.StripeClients, Config: r.Config, Merchants: r.Merchants, DB: r.DB, NMIClients: r.NMIClients, DataLinkPacing: r.DataLinkPacing(), Log: r.Log},
			Log:     r.Log,
		}
		r.Verifier.Start()
	}
	refresh, paced := r.refreshWorkers(clock)
	if err := addWorker(workers, refresh); err != nil {
		return fmt.Errorf("add provider refresh worker: %w", err)
	}
	if err := addWorker(workers, paced); err != nil {
		return fmt.Errorf("add paced provider refresh worker: %w", err)
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
		Log:    r.Log,
	}); err != nil {
		return fmt.Errorf("add cleanup expired data worker: %w", err)
	}
	// Stored cards a vault must be asked about: one refresh per card, queued on
	// an expired or reissued decline and ahead of each renewal.
	var updaters paymentmethods.AccountUpdaters
	if r.CollectionResolver != nil {
		updaters = r.CollectionResolver
	}
	if err := addWorker(workers, &riverjobs.StoredCardRefreshWorker{DB: r.DB, Clock: clock, Updaters: updaters, Intents: r.intentRunner(intentRegistry, clock), Lifecycle: r.SubscriptionLifecycleService, Log: r.Log}); err != nil {
		return fmt.Errorf("add stored card refresh worker: %w", err)
	}
	// A balance's look, queued by the change that crossed one of its lines
	// and for the instant its last look named.
	if err := addWorker(workers, &riverjobs.UsageErasureWorker{DB: r.DB}); err != nil {
		return fmt.Errorf("add usage erasure worker: %w", err)
	}
	if err := addWorker(workers, &riverjobs.BalanceLookWorker{DB: r.DB, Money: r.MoneyService}); err != nil {
		return fmt.Errorf("add balance look worker: %w", err)
	}
	// A customer's convergence, queued by each mutation that changed it.
	if err := addWorker(workers, &riverjobs.CustomerConvergeWorker{
		DB: r.DB, Config: r.Config, Clock: clock, Alerts: r.AlertService,
	}); err != nil {
		return fmt.Errorf("add customer converge worker: %w", err)
	}
	// A PSP's dispute monitoring programs, queued by each chargeback and
	// fraud report, and by a sale while one of its findings is open.
	if err := addWorker(workers, &riverjobs.DisputeMonitorWorker{DB: r.DB, Disputes: r.Disputes}); err != nil {
		return fmt.Errorf("add dispute monitor worker: %w", err)
	}
	// What a chargeback or fraud report calls for, once the gates admit it.
	if err := addWorker(workers, &riverjobs.DisputeResponseWorker{DB: r.DB, Disputes: r.Disputes}); err != nil {
		return fmt.Errorf("add dispute response worker: %w", err)
	}
	// Health findings and timed access lapses, for merchants with activity.
	if err := addWorker(workers, &riverjobs.MerchantHealthWorker{
		DB: r.DB, Config: r.Config, Clock: clock, Alerts: r.AlertService,
	}); err != nil {
		return fmt.Errorf("add merchant health worker: %w", err)
	}
	// Accepted operations carry their own durable River lifecycle job.
	if err := addWorker(workers, &riverjobs.ProviderOperationWorker{
		DB: r.DB, Config: r.Config, Clock: clock, Runner: r.intentRunner(intentRegistry, clock),
		Log: r.Log,
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
		Log:                          r.Log,
	}); err != nil {
		return fmt.Errorf("add subscription converge worker: %w", err)
	}
	// Catalog objects a Stripe event announced, read by id.
	if err := addWorker(workers, &riverjobs.ProviderObjectObserveWorker{Objects: r.ProviderObjects, Log: r.Log}); err != nil {
		return fmt.Errorf("add provider object observe worker: %w", err)
	}
	// PSP imports: a read-only pull of a PSP, then its landing.
	if err := addWorker(workers, &riverjobs.PSPImportWorker{Importer: r.PSPImporter}); err != nil {
		return fmt.Errorf("add psp import worker: %w", err)
	}
	// Declared charges without an amount, read from their provider by the
	// job their import queued.
	if err := addWorker(workers, &riverjobs.ChargeAttributionWorker{
		DB: r.DB, Clock: clock, NMIResolver: r.CollectionResolver, Rails: r.RailConfigs,
	}); err != nil {
		return fmt.Errorf("add charge attribution worker: %w", err)
	}
	// Each Stripe PSP's webhook endpoint, queued when the PSP or the boot's
	// configuration changes, at a superseded endpoint's retirement, and daily.
	if err := addWorker(workers, &riverjobs.StripeWebhookReconcileWorker{
		StripeClients: r.StripeClients,
		DB:            r.DB,
		Config:        r.Config,
		Merchants:     r.Merchants,
		Clock:         clock,
		Reload:        r.reloadMerchantConfig,
		Log:           r.Log,
	}); err != nil {
		return fmt.Errorf("add stripe webhook reconcile worker: %w", err)
	}
	// A merchant purge's Vault cleanup and event store deletion, queued with
	// the purge.
	if err := addWorker(workers, &riverjobs.MerchantSecretCleanupWorker{Merchants: r.Merchants}); err != nil {
		return fmt.Errorf("add merchant secret cleanup worker: %w", err)
	}
	if err := addWorker(workers, &riverjobs.MerchantUsagePurgeWorker{DB: r.DB}); err != nil {
		return fmt.Errorf("add merchant usage purge worker: %w", err)
	}
	// Customer messages: each pending one's job, queued by the dispatcher or
	// its periodic backstop, and the job that sends it.
	if err := addWorker(workers, &riverjobs.CustomerMessageDispatchWorker{Messages: r.Messages}); err != nil {
		return fmt.Errorf("add customer message dispatch worker: %w", err)
	}
	if err := addWorker(workers, &riverjobs.CustomerMessageWorker{Messages: r.Messages}); err != nil {
		return fmt.Errorf("add customer message worker: %w", err)
	}
	// MRR risk: the backstop of the drain each queued change wakes.
	if err := addWorker(workers, &riverjobs.MRRRiskWorker{Risk: r.MRRRisk}); err != nil {
		return fmt.Errorf("add mrr risk worker: %w", err)
	}
	if err := addWorker(workers, &riverjobs.ContactErasureWorker{DB: r.DB, Admissions: r.DestructiveAdmissions}); err != nil {
		return fmt.Errorf("add contact erasure worker: %w", err)
	}
	// Usage orders: due accounts billed, due collections queued as
	// operations; a full page continues in its own job.
	usage := &riverjobs.UsageOrderWorker{DB: r.DB, Money: r.MoneyService, Config: r.Config, Clock: clock, Log: r.Log}
	if err := addWorker(workers, usage); err != nil {
		return fmt.Errorf("add usage order worker: %w", err)
	}
	// Every due scan, in turn, at the top of each hour. Renewals, retries,
	// delinquency and lapses are critical: a day overdue fails job progress.
	dunning := &riverjobs.DunningWorker{DB: r.DB, Config: r.Config, Clock: clock, NMIResolver: r.CollectionResolver, EngineCollections: r.MoneyService, Lifecycle: r.SubscriptionLifecycleService, Log: r.Log}
	balances := &riverjobs.BalanceWatchWorker{DB: r.DB, Money: r.MoneyService, Log: r.Log}
	delinquency := &riverjobs.DelinquencyWorker{DB: r.DB, Clock: clock, Log: r.Log}
	expiry := &riverjobs.OrderExpiryWorker{DB: r.DB, Orders: r.Orders, Clock: clock}
	// Overdue NMI rebills are probed.
	rebills := &riverjobs.RebillWatchWorker{DB: r.DB, Config: r.Config, Clock: clock, NMIResolver: r.CollectionResolver, Lifecycle: r.SubscriptionLifecycleService, Log: r.Log}
	// Each NMI PSP's own history, as monthly aggregates.
	nmiHistory := &riverjobs.NMIHistoryWorker{DB: r.DB, Clock: clock, NMIResolver: r.CollectionResolver, Log: r.Log}
	// Undeclared accounts alert their operator again while unresolved.
	realerts := &riverjobs.ConfigRealertWorker{DB: r.DB, Clock: clock, Alerts: r.AlertService, Reload: r.reloadMerchantConfig}
	storedCards := &riverjobs.StoredCardRefreshDueWorker{DB: r.DB, Clock: clock, Merchants: func() *merchants.Service { return r.Merchants }, Posture: r.Posture(), Log: r.Log}
	closing := &riverjobs.UsageCloseWorker{DB: r.DB, Money: r.MoneyService, Log: r.Log}
	// Deferred revenue recognised at each month boundary in the merchant's
	// accounting timezone, at each schedule's end and at an early end.
	recognition := &riverjobs.RevenueRecognitionWorker{DB: r.DB, Clock: clock, Log: r.Log}
	scans := []riverjobs.Scan{
		// A month's usage posts once its month ends, before anything bills it.
		{Kind: riverjobs.KindUsageClose, Run: closing.Pass, DueBy: closing.DueBy},
		{Kind: riverjobs.KindDunning, Run: dunning.Pass, DueBy: dunning.DueBy},
		{Kind: riverjobs.KindSubscriptionVerificationRetry, Run: r.Verifier.RetryDue},
		{Kind: riverjobs.KindBalanceWatch, Run: balances.Pass, DueBy: balances.DueBy},
		{Kind: riverjobs.KindDelinquency, Run: delinquency.Pass, DueBy: delinquency.DueBy},
		{Kind: riverjobs.KindOrderExpiry, Run: expiry.Pass, DueBy: expiry.DueBy},
		{Kind: riverjobs.KindRevenueRecognition, Run: recognition.Pass, DueBy: recognition.DueBy},
	}
	if r.declares(riverjobs.WithUsageBilling) {
		scans = append(scans, riverjobs.Scan{Kind: money.KindUsageOrders, Run: usage.Pass, DueBy: usage.DueBy})
	}
	scans = append(scans,
		riverjobs.Scan{Kind: riverjobs.KindRebillWatch, Run: rebills.Pass},
		riverjobs.Scan{Kind: riverjobs.KindNMIHistory, Run: nmiHistory.Pass},
		riverjobs.Scan{Kind: riverjobs.KindConfigRealerts, Run: realerts.Pass},
		riverjobs.Scan{Kind: riverjobs.KindStoredCardRefreshDue, Run: storedCards.Pass},
		riverjobs.Scan{Kind: riverjobs.KindOperationWakes, Run: (&riverjobs.OperationWakes{DB: r.DB, Clock: clock, Log: r.Log}).Pass},
		// Cancellations at period end leave MRR at their end.
		riverjobs.Scan{Kind: riverjobs.KindMRRDue, Run: (&riverjobs.MRRDueWorker{DB: r.DB, Clock: clock, Log: r.Log}).Pass},
		// Stripe's fees on the charges, refunds and disputes recorded since.
		riverjobs.Scan{Kind: riverjobs.KindStripeFees, Run: (&fees.StripeReader{DB: r.DB, Rails: r.RailConfigs, Stripe: r.StripeClients, Log: r.Log}).Pass},
	)
	for i := range scans {
		scans[i].Timeout = riverjobs.ScanBudget(scans[i].Kind)
	}
	if err := addWorker(workers, &riverjobs.BillingPassWorker{DB: r.DB, Scans: scans, Clock: clock, Log: r.Log}); err != nil {
		return fmt.Errorf("add billing pass worker: %w", err)
	}
	r.riverCompositionMu.Lock()
	r.riverWorkers = workers.added
	r.riverCompositionMu.Unlock()
	return nil
}

// workerSet is a River registry and the workers added to it.
type workerSet struct {
	river *river.Workers
	added []any
}

// addWorker registers an OpenRails worker. Its timeout is its kind's budget,
// which job health reads; the client's JobTimeout is the host's.
func addWorker[T river.JobArgs](workers *workerSet, worker river.Worker[T]) error {
	var args T
	want := riverjobs.Timeout(args.Kind())
	if want == 0 {
		return fmt.Errorf("%s is not an OpenRails job kind", args.Kind())
	}
	if got := worker.Timeout(&river.Job[T]{JobRow: &rivertype.JobRow{Kind: args.Kind()}}); got != want {
		return fmt.Errorf("%s: timeout %s is not its kind's budget %s", args.Kind(), got, want)
	}
	if err := river.AddWorkerSafely(workers.river, jobLogged[T]{worker}); err != nil {
		return err
	}
	workers.added = append(workers.added, worker)
	return nil
}

// jobLogged logs its worker's job with the job's kind, id and attempt.
type jobLogged[T river.JobArgs] struct{ river.Worker[T] }

func (w jobLogged[T]) Work(ctx context.Context, job *river.Job[T]) error {
	return w.Worker.Work(logx.With(ctx, "job_kind", job.Kind, "job_id", job.ID, "attempt", job.Attempt), job)
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
		intents.NewStripeResumeHandler(r.DB, r.Config, r.RailConfigs, r.StripeClients, clock),
		intents.NewStripePaymentMethodDomainHandler(r.Config, r.RailConfigs, r.StripeClients),
		rebill,
		// Order collection rides the intent ledger like every other money
		// mover.
		money.NewOrderCollectionHandler(r.DB, r.MoneyCharger, r.CollectionResolver, r.Posture(), clock),
		money.NewSubscriptionCollectionHandler(r.DB, r.CollectionResolver, r.Config, clock),
		// Write-through kinds live with their domain services, which every
		// worker runtime has (validateBillingWorkerRuntime): an accepted
		// operation of an unregistered kind would never run.
		payment.NewNMISaleIntentHandler(r.Log, r.PurchaseService.NMISaleService),
		payment.NewInitialMembershipIntentHandler(r.Log, r.PurchaseService, r.CollectionResolver),
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
	if r.CollectionResolver != nil {
		registry.Register(&riverjobs.AccountUpdaterHandler{DB: r.DB, Clock: clock, Updaters: r.CollectionResolver, Lifecycle: r.SubscriptionLifecycleService, Policy: intents.DefaultBackoff, Log: r.Log})
	}
	// Solana recurring pull: signature write-ahead before the send, chain-read
	// verification after it. The verify leg reads the chain with the intent's
	// merchant-armed client.
	solanaPull := pull.Handler{DB: r.DB, Clock: clock, Lifecycle: r.SubscriptionLifecycleService, Store: intents.NewStore(r.DB), Log: r.Log}
	if r.SolanaCranker != nil {
		solanaPull.Cranker = r.SolanaCranker
		solanaPull.Gas = r.checkSolanaCrankerGas
	}
	if r.SolanaRPCResolver != nil {
		solanaPull.Chain = r.SolanaRPCResolver.ChainReader()
	}
	registry.Register(pull.NewHandler(solanaPull))
	// A Solana plan change is found on chain by the reference it was prepared with.
	registry.Register(r.SolanaPlanChanges(clock))
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
		Log:         r.Log,
	}
	return runner
}

// StripeEngines is the collection resolver as the Stripe engine reads it; nil
// when the runtime has none.
func (r *Runtime) StripeEngines() intents.StripeEngineServiceResolver {
	if r.CollectionResolver == nil {
		return nil
	}
	return r.CollectionResolver
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
	if r.Messages == nil {
		return fmt.Errorf("billing worker runtime customer messages are required")
	}
	if r.MRRRisk == nil {
		return fmt.Errorf("billing worker runtime mrr risk is required")
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

// refreshWorkers are the provider refresh's workers: an operator request runs
// bounded window pulls, CCBill DataLink and reconciliation for the merchant,
// and a paced PSP's refresh (a live CCBill account) reads a request per slot
// of the runtime's DataLinkPacing.
func (r *Runtime) refreshWorkers(clock clockwork.Clock) (*riverjobs.ProviderRefreshWorker, *riverjobs.PacedProviderRefreshWorker) {
	refresh := &riverjobs.ProviderRefreshWorker{
		StripeClients:  r.StripeClients,
		DB:             r.DB,
		Config:         r.Config,
		Clock:          clock,
		Merchants:      r.Merchants, // per-merchant store-armed pulls
		DeferDelete:    r.DeferredDeletes,
		Contacts:       r.Contacts,
		Alerts:         r.AlertService, // requires_review findings -> operator notifications
		NMIClients:     r.NMIClients,
		DataLinkPacing: r.DataLinkPacing(),
		Verifier:       r.Verifier,
		Disputes:       r.Disputes,
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
		Log: r.Log,
	}
	return refresh, &riverjobs.PacedProviderRefreshWorker{Refresh: refresh}
}
