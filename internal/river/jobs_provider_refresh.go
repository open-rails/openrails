package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhookhealth"
	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/internal/writeposture"
)

const (
	KindProviderRefresh         = "openrails.provider_refresh"
	KindProviderRefreshMerchant = "openrails.provider_refresh_merchant"

	// QueueProviderRefresh bounds refresh concurrency in standalone: a small
	// MaxWorkers cap keeps many merchants from stampeding a provider's API
	// budget. Embedded hosts run the kind on QueueBilling.
	QueueProviderRefresh = "provider_refresh"

	providerRefreshDomainEvents = "events"
	defaultRefreshWindow        = 24 * time.Hour
	defaultRefreshSafetyLag     = 5 * time.Minute
	defaultRefreshMaxWindows    = 8
	// defaultRefreshStagger spreads the scheduler's fan-out so merchant pull
	// windows don't align on the tick instant.
	defaultRefreshStagger = 30 * time.Minute
)

// ProviderRefreshArgs is the periodic scheduler kind: it fans out one
// ProviderRefreshMerchantArgs job per merchant.
type ProviderRefreshArgs struct{}

func (ProviderRefreshArgs) Kind() string { return KindProviderRefresh }

// ProviderRefreshMerchantArgs is one merchant's refresh job.
type ProviderRefreshMerchantArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	// Requested marks a host-requested refresh: it must observe provider state
	// from after the request, so it never merges into a scheduled pass that
	// may already be running.
	Requested bool `json:"requested,omitempty" river:"unique"`
}

func (ProviderRefreshMerchantArgs) Kind() string { return KindProviderRefreshMerchant }

// providerRefreshUniqueStates is the default unique set minus completed: one
// in-flight job per merchant, re-enqueueable next tick, so overlapping
// scheduler passes never double-refresh a merchant.
var providerRefreshUniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// EnqueueMerchantRefresh requests the merchant's provider refresh now on
// queue. An in-flight refresh absorbs the request; one scheduled for later
// (the staggered periodic tick) is started now.
func EnqueueMerchantRefresh(ctx context.Context, client *river.Client[pgx.Tx], merchantID uuid.UUID, queue string) (jobID int64, alreadyQueued bool, err error) {
	if client == nil {
		return 0, false, errors.New("provider refresh: no River producer")
	}
	if queue == "" {
		queue = QueueProviderRefresh
	}
	res, err := client.Insert(ctx, ProviderRefreshMerchantArgs{MerchantID: merchantID, Requested: true}, &river.InsertOpts{
		Queue:      queue,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: providerRefreshUniqueStates},
	})
	if err != nil {
		return 0, false, err
	}
	if res.UniqueSkippedAsDuplicate && res.Job.State == rivertype.JobStateScheduled {
		if _, err := client.JobRetry(ctx, res.Job.ID); err != nil {
			return 0, false, err
		}
	}
	return res.Job.ID, res.UniqueSkippedAsDuplicate, nil
}

// refreshJobInserter is the slice of river.Client the scheduler uses (test seam).
type refreshJobInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// ProviderRefreshSchedulerWorker is the periodic umbrella: it enqueues one
// refresh job per active merchant that has a PSP, spread evenly over Stagger.
// River supplies per-merchant error isolation and retries; the bounded queue
// caps refresh concurrency.
type ProviderRefreshSchedulerWorker struct {
	river.WorkerDefaults[ProviderRefreshArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock

	// MerchantQueue is where per-merchant jobs land ("" = QueueProviderRefresh).
	MerchantQueue string
	// Stagger is the fan-out spread window (0 = defaultRefreshStagger). Slot 0
	// is immediate, so a single-merchant (embedded) boot refreshes right away.
	Stagger time.Duration

	// Test seams; nil = production defaults.
	Inserter        refreshJobInserter
	ListMerchants   func(ctx context.Context) ([]uuid.UUID, error)
	HasRailAccounts func(ctx context.Context, mid uuid.UUID) (bool, error)
}

func (ProviderRefreshSchedulerWorker) Kind() string { return KindProviderRefresh }

func (w *ProviderRefreshSchedulerWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *ProviderRefreshSchedulerWorker) stagger() time.Duration {
	if w.Stagger > 0 {
		return w.Stagger
	}
	return defaultRefreshStagger
}

func (w *ProviderRefreshSchedulerWorker) merchantQueue() string {
	if w.MerchantQueue != "" {
		return w.MerchantQueue
	}
	return QueueProviderRefresh
}

func (w *ProviderRefreshSchedulerWorker) listMerchants(ctx context.Context) ([]uuid.UUID, error) {
	if w.ListMerchants != nil {
		return w.ListMerchants(ctx)
	}
	return w.DB.GenDirectory().ListActiveMerchantIDs(ctx)
}

// merchantHasRailAccounts reports whether the merchant declares any PSP.
// Archived PSPs count (drain pulls still arm). Environment is not filtered: a
// wrong-environment PSP enqueues a job that arms nothing (fail open, cheap).
func (w *ProviderRefreshSchedulerWorker) merchantHasRailAccounts(ctx context.Context, mid uuid.UUID) (bool, error) {
	if w.HasRailAccounts != nil {
		return w.HasRailAccounts(ctx, mid)
	}
	var exists bool
	mctx := merchant.WithID(ctx, billing.MerchantID(mid))
	err := w.DB.MerchantTx(mctx, func(tctx context.Context, tx pgx.Tx) error {
		// The explicit merchant_id predicate is the scope.
		var err error
		exists, err = gen.New(tx).MerchantHasPSPs(tctx, mid)
		return err
	})
	return exists, err
}

func (w *ProviderRefreshSchedulerWorker) Work(ctx context.Context, _ *river.Job[ProviderRefreshArgs]) error {
	inserter := w.Inserter
	if inserter == nil {
		client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
		if err != nil {
			return fmt.Errorf("provider refresh scheduler: river client: %w", err)
		}
		inserter = client
	}
	merchantIDs, err := w.listMerchants(ctx)
	if err != nil {
		return fmt.Errorf("provider refresh scheduler: list merchants: %w", err)
	}
	// Shuffle kills the ListActiveMerchantIDs order bias: no merchant is
	// systematically last in every window.
	// #nosec G404 -- this is scheduling fairness, not a security-sensitive random choice.
	rand.Shuffle(len(merchantIDs), func(i, j int) { merchantIDs[i], merchantIDs[j] = merchantIDs[j], merchantIDs[i] })

	now := w.now()
	var spacing time.Duration
	if n := len(merchantIDs); n > 0 {
		spacing = w.stagger() / time.Duration(n)
	}
	// A merchant with no PSP has nothing to refresh: skip it before enqueue.
	var enqueued, deduped, skipped, failed, slot int
	for _, mid := range merchantIDs {
		progress.Mark(ctx, "refresh scheduler merchant "+mid.String())
		ok, err := w.merchantHasRailAccounts(ctx, mid)
		if err != nil {
			// Fail open: a broken predicate must not starve refresh.
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: accounts predicate failed; enqueueing anyway")
		} else if !ok {
			skipped++
			continue
		}
		res, err := inserter.Insert(ctx, ProviderRefreshMerchantArgs{MerchantID: mid}, &river.InsertOpts{
			Queue:       w.merchantQueue(),
			ScheduledAt: now.Add(time.Duration(slot) * spacing),
			UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: providerRefreshUniqueStates},
		})
		slot++
		if err != nil {
			failed++
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: enqueue merchant refresh failed")
			continue
		}
		if res != nil && res.UniqueSkippedAsDuplicate {
			deduped++
		} else {
			enqueued++
		}
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"merchants":           len(merchantIDs),
		"enqueued":            enqueued,
		"deduped":             deduped,
		"skipped_no_accounts": skipped,
		"enqueue_errors":      failed,
		"queue":               w.merchantQueue(),
		"stagger":             cadence.FormatDuration(w.stagger()),
	}).Info("Provider Refresh: scheduled merchant refresh jobs")
	if failed > 0 {
		// Retry the scheduler; per-merchant unique keys make the rerun idempotent.
		return fmt.Errorf("provider refresh scheduler: %d/%d enqueues failed", failed, len(merchantIDs))
	}
	return nil
}

// ProviderRefreshWorker is one merchant's provider-read refresh. It keeps
// provider truth fresh without remote mutations: bounded event pulls with
// durable watermarks, the unknown-cohort reconcile (the one per-subscription
// verification path) and the CCBill DataLink lane. Provider outages leave
// watermarks in place for the next pass. Fetchers and probers arm per
// merchant, inside its scope, from its PSPs; nothing credentialed is cached
// across merchants.
type ProviderRefreshWorker struct {
	StripeClients *stripeapi.Factory
	river.WorkerDefaults[ProviderRefreshMerchantArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	// Merchants resolves per-merchant PSPs and scoped secrets. nil = nothing
	// arms.
	Merchants   *merchants.Service
	DeferDelete subscriptions.ProviderCancelScheduler
	// Contacts supplies customer emails for identity matching.
	Contacts identity.Directory
	// Alerts sends requires_review findings to the operator notification
	// store. nil = no-op.
	Alerts *alerting.Service

	// PullEndpoints overrides provider base URLs on store-armed clients
	// (fake-provider test seam).
	PullEndpoints reconcile.ProviderEndpoints
	NMIClients    *railresolve.NMIFactory
	// Verifier reads the unverified NMI rows; nil leaves them to the
	// unknown-cohort reconcile.
	Verifier *reconcile.Verifier
	// Positive invoice recovery does not authorize provider or lifecycle writes.
	RecoverInvoicePayment func(context.Context, uuid.UUID, string) error

	Window     time.Duration
	SafetyLag  time.Duration
	MaxWindows int
}

func (ProviderRefreshWorker) Kind() string { return KindProviderRefreshMerchant }

func (w *ProviderRefreshWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *ProviderRefreshWorker) Work(ctx context.Context, job *river.Job[ProviderRefreshMerchantArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("provider refresh: db not configured")
	}
	mid := job.Args.MerchantID
	if mid == uuid.Nil {
		return fmt.Errorf("provider refresh: merchant_id required")
	}

	logger := log.WithContext(ctx).WithField("worker", KindProviderRefreshMerchant).WithField("merchant_id", mid)
	stats := providerRefreshStats{Merchants: 1}

	// One refresh per merchant at a time, across every replica.
	release, held, err := w.lockMerchant(ctx, mid)
	if err != nil {
		return err
	}
	if !held {
		return river.JobSnooze(5 * time.Second)
	}
	defer release()

	// Fetchers and per-subscription probers arm per merchant, inside its scope.
	// A rail that cannot arm is absent for that merchant (its WARN names
	// merchant/rail/secret); the other rails keep pulling.
	builder := reconcile.MerchantFetcherBuilder{
		StripeClients: w.StripeClients,
		Config:        w.Config,
		Merchants:     w.Merchants,
		DB:            w.DB,
		Endpoints:     w.PullEndpoints,
		NMIClients:    w.NMIClients,
	}

	err = w.refreshMerchant(ctx, mid, builder, &stats, logger)

	logger.WithFields(log.Fields{
		"merchants":        stats.Merchants,
		"windows":          stats.Windows,
		"providers":        stats.Providers,
		"applied_changes":  stats.AppliedChanges,
		"new_findings":     stats.NewFindings,
		"updated_findings": stats.UpdatedFindings,
		"watermark_errors": stats.WatermarkErrors,
		"provider_errors":  stats.ProviderErrors,
		"ccbill_errors":    stats.CCBillErrors,
		"converge_errors":  stats.ConvergeErrors,
		"lane_errors":      stats.LaneErrors,
		"gated":            stats.Gated,
	}).Info("Provider Refresh: pass completed")
	if err != nil {
		// Merchant connection failed — nothing ran; river retries with backoff.
		// Lane failures stay best-effort (watermarks resume them next tick).
		return fmt.Errorf("provider refresh: merchant %s: %w", mid, err)
	}
	if stats.More {
		return river.JobSnooze(time.Second)
	}
	if stats.ProviderErrors+stats.WatermarkErrors+stats.LaneErrors+stats.ConvergeErrors+stats.CCBillErrors > 0 {
		return fmt.Errorf("provider refresh incomplete: provider=%d watermark=%d lane=%d", stats.ProviderErrors, stats.WatermarkErrors, stats.LaneErrors)
	}
	return nil
}

// refreshMerchant reads and repairs each account's financial mirror first.
// Policy-held lifecycle repairs remain separate from that observation phase.
func (w *ProviderRefreshWorker) refreshMerchant(ctx context.Context, mid uuid.UUID, builder reconcile.MerchantFetcherBuilder, stats *providerRefreshStats, logger *log.Entry) error {
	gate := destructive.New(w.DB)
	mctx := merchant.WithID(ctx, billing.MerchantID(mid))
	if err := w.DB.RunInMerchantConn(mctx, func(tctx context.Context) error {
		// Provider observation and positive receipt recovery continue when
		// remote writes or destructive repairs are held. Only the latter needs
		// the operator's destructive arming; readonly never changes its mode.
		verdict := gate.Check(tctx, mid)
		overwrite := verdict.Allowed && verdict.EnforceArmed && !(writeposture.View{Config: w.Config, DB: w.DB}).Posture(tctx, mid).ReadOnly()
		if !overwrite {
			stats.Gated++
		}
		accounts, err := w.DB.Gen(tctx).ListPSPsForMerchant(tctx, mid)
		if err != nil {
			return err
		}
		var res providerRefreshMerchantResult
		for _, account := range accounts {
			if account.Environment != config.ExpectedProviderEnvironment(config.IsTestMode(w.Config)) {
				continue
			}
			provider := reconcile.Provider(account.Rail)
			accountBuilder := builder
			accountBuilder.AccountIDs = map[reconcile.Provider]string{provider: account.AccountID}
			armed := accountBuilder.Build(tctx, billing.MerchantID(mid))
			fetcher, ok := armed.Fetchers[provider]
			if !ok || armed.Coverage[provider].Binding.ID != account.ID {
				if providerrecovery.CheckPSP(tctx, w.DB, mid, account.ID, w.now()) != nil {
					stats.ProviderErrors++
				}
				continue
			}
			accountOverwrite := overwrite && providerrecovery.CheckPSP(tctx, w.DB, mid, account.ID, w.now()) == nil
			mutations := &reconcile.LocalMutationPolicy{Insert: true, Overwrite: accountOverwrite}
			accountResult := w.runEventRefresh(tctx, mid, reconcile.ModeEnforce, mutations, armed.Coverage, map[reconcile.Provider]reconcile.RailFetcher{provider: fetcher})
			res.add(accountResult.providerRefreshProviderResult)
			stats.add(accountResult)
			if accountResult.Windows > 0 {
				if err := gate.RecordFirstPull(tctx, mid, w.now()); err != nil {
					stats.LaneErrors++
				}
			}
		}
		if !overwrite || providerrecovery.CheckMerchant(tctx, w.DB, mid, w.now()) != nil {
			return nil
		}
		armed := builder.Build(tctx, billing.MerchantID(mid))

		// The DataLink lane reactivates local rows, so it runs only once the
		// merchant is armed for enforcement.
		if err := w.runCCBillDataLinkLane(db.WithPSPID(tctx, armed.Coverage[reconcile.ProviderCCBill].Binding.ID), armed.CCBillDataLink); err != nil {
			stats.CCBillErrors++
			logger.WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: CCBill DataLink lane failed")
		}

		// Confirmed-absence gate: a completed exhaustive pull proves a source
		// domain; mark it reconciled before convergence so held excess
		// repairs can proceed.
		if len(res.Proofs) > 0 {
			if flipped, err := reconcile.MarkReconciledSourceDomains(tctx, w.DB.Gen(tctx), mid, res.Proofs); err != nil {
				stats.LaneErrors++
				logger.WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: mark reconciled domains failed")
			} else if len(flipped) > 0 {
				logger.WithFields(log.Fields{"merchant_id": mid, "domains": flipped}).Info("Provider Refresh: source domains proven reconciled")
			}
		}
		if res.Changed {
			if err := w.runConvergence(tctx, mid); err != nil {
				stats.ConvergeErrors++
				logger.WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: scoped convergence failed")
			}
		}
		// Reconcile the `unknown` cohort against the provider. A rail without
		// a working fetcher leaves its subscriptions `unknown` for the next pass.
		if err := w.runUnknownReconcile(tctx, mid, armed.Fetchers, armed.Probers); err != nil {
			stats.LaneErrors++
			logger.WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: unknown-cohort reconcile failed")
		}
		return nil
	}); err != nil {
		stats.LaneErrors++
		logger.WithError(err).WithField("merchant_id", mid).Error("Provider Refresh: merchant connection failed")
		return err
	}
	return nil
}

func (w *ProviderRefreshWorker) runCCBillDataLinkLane(ctx context.Context, dataLink *ccbill.DataLinkClient) error {
	return CCBillReconciler{
		Clock:    w.Clock,
		DB:       w.DB,
		DataLink: dataLink,
	}.Run(ctx)
}

// runUnknownReconcile resolves the merchant's `unknown` subscriptions against
// the provider (one windowed bulk pull per rail, probes as fallback),
// backfilling missing charges; with a Verifier, NMI rows go to it instead. The
// lifecycle service needs only local writes, so it is built with nil deps.
// Best-effort: a rail whose pull fails leaves its subscriptions `unknown` for
// the next pass.
func (w *ProviderRefreshWorker) runUnknownReconcile(ctx context.Context, mid uuid.UUID, fetchers map[reconcile.Provider]reconcile.RailFetcher, probers map[reconcile.Provider]reconcile.SubscriptionProber) error {
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	lc := subscriptions.NewSubscriptionLifecycleService(w.DB, nil, nil, nil, nil, nil, clock)
	lc.SetConfig(w.Config)
	if w.DeferDelete != nil {
		// A stale-decline cancel must durably queue the deferred NMI delete;
		// without this the lifecycle WARNs and the remote keeps retrying.
		lc.SetProviderCancelScheduler(w.DeferDelete)
	}
	opts := reconcile.UnknownReconcileOptions{}
	var verifyErr error
	if w.Verifier != nil {
		opts.SkipRails = []string{string(reconcile.ProviderNMI)}
		verifyErr = w.Verifier.Pass(ctx, billing.MerchantID(mid))
	}
	res, err := reconcile.ReconcileUnknownCohort(ctx, w.DB, lc, fetchers, probers, billing.MerchantID(mid), w.now(), opts)
	err = errors.Join(verifyErr, err)
	if res.Held > 0 {
		log.WithContext(ctx).WithFields(log.Fields{"merchant_id": mid, "held": res.Held}).
			Error("Provider Refresh: unknown-cohort cancellations withheld by a pass-level guard; a requires_review finding is open")
	}
	if res.Renewed+res.Adopted+res.PastDue+res.Canceled+res.Backfilled > 0 || len(res.RailErrors) > 0 {
		log.WithContext(ctx).WithFields(log.Fields{
			"merchant_id": mid, "renewed": res.Renewed, "adopted": res.Adopted, "past_due": res.PastDue,
			"canceled": res.Canceled, "still_unknown": res.StillUnknown, "probed": res.Probed,
			"backfilled": res.Backfilled, "psp_customers": res.RailCustomers, "rail_errors": len(res.RailErrors),
		}).Info("Provider Refresh: unknown-cohort reconcile")
	}
	return err
}

func (w *ProviderRefreshWorker) runConvergence(ctx context.Context, mid uuid.UUID) error {
	engine := converge.NewConvergeEngine(w.DB)
	engine.Now = func() time.Time { return w.now() }
	if w.Alerts != nil {
		// A nil *alerting.Service boxed into the FindingNotifier interface
		// would panic on first use.
		engine.Notifier = w.Alerts
	}
	_, err := engine.Converge(ctx, converge.Scope{Merchant: billing.MerchantID(mid)})
	return err
}

func (w *ProviderRefreshWorker) runEventRefresh(ctx context.Context, mid uuid.UUID, mode reconcile.Mode, mutations *reconcile.LocalMutationPolicy, coverage map[reconcile.Provider]reconcile.PSPCoverage, fetchers map[reconcile.Provider]reconcile.RailFetcher) providerRefreshMerchantResult {
	result := providerRefreshMerchantResult{}
	providers := refreshProviders(fetchers)
	for _, provider := range providers {
		// Watermarks and mirror rows are per PSP: pulling one account must
		// never advance another's watermark or land unattributed.
		binding := coverage[provider].Binding
		if binding.ID == uuid.Nil {
			result.ProviderErrors++
			log.WithContext(ctx).WithFields(log.Fields{"merchant_id": mid, "provider": provider}).
				Error("Provider Refresh: rail armed without a resolved PSP; refusing an unattributed pull")
			continue
		}
		providerRes := w.runProviderEventWindows(ctx, mid, provider, mode, mutations, coverage, binding, fetchers)
		result.add(providerRes)
	}
	return result
}

// refreshProviders selects the pull lanes to run, sorted: the armed fetchers
// whose rail is production-ready.
func refreshProviders(fetchers map[reconcile.Provider]reconcile.RailFetcher) []reconcile.Provider {
	allowed := map[reconcile.Provider]bool{
		reconcile.ProviderNMI:    true,
		reconcile.ProviderStripe: true,
		reconcile.ProviderCCBill: true,
		reconcile.ProviderSolana: true,
	}
	providers := make([]reconcile.Provider, 0, len(fetchers))
	for provider := range fetchers {
		if allowed[provider] {
			providers = append(providers, provider)
		}
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i] < providers[j] })
	return providers
}

func (w *ProviderRefreshWorker) runProviderEventWindows(ctx context.Context, mid uuid.UUID, provider reconcile.Provider, mode reconcile.Mode, mutations *reconcile.LocalMutationPolicy, coverage map[reconcile.Provider]reconcile.PSPCoverage, binding reconcile.PSPBinding, fetchers map[reconcile.Provider]reconcile.RailFetcher) providerRefreshProviderResult {
	out := providerRefreshProviderResult{Providers: 1}
	pspID := binding.ID
	bindings := map[reconcile.Provider]reconcile.PSPBinding{provider: binding}
	now := w.now()
	horizon := now.Add(-w.safetyLag())
	if !horizon.After(time.Time{}) {
		return out
	}

	qualifiedRecovery := provider == reconcile.ProviderNMI || provider == reconcile.ProviderStripe
	var since time.Time
	var err error
	if qualifiedRecovery {
		since, err = w.loadAppliedWatermark(ctx, mid, pspID)
	} else {
		// Other rails retain their observation cursor and existing lanes;
		// they do not acquire the card-account financial recovery contract.
		since, err = w.DB.Gen(ctx).GetPSPRefreshWatermark(ctx, gen.GetPSPRefreshWatermarkParams{MerchantID: mid, PspID: pspID})
		if errors.Is(err, pgx.ErrNoRows) {
			since, err = horizon.Add(-90*24*time.Hour), nil
		}
	}
	if err != nil {
		out.WatermarkErrors++
		log.WithContext(ctx).WithError(err).WithField("provider", provider).Warn("Provider Refresh: load watermark failed")
		return out
	}
	// Repeat a bounded window for late provider indexing. This is recovery
	// overlap, not a claim that the provider can never publish older history.
	if qualifiedRecovery {
		since = since.Add(-w.window())
	}
	if !since.Before(horizon) {
		return out
	}

	engine := reconcile.NewEngine(w.DB, w.Config, w.Contacts, fetchers, w.DeferDelete)
	engine.RecoverInvoicePayment = w.RecoverInvoicePayment
	engine.Now = func() time.Time { return now }
	if w.Alerts != nil {
		engine.Notifier = w.Alerts // nil-check: see runConvergence
	}
	maxWindows := w.maxWindows()
	for i := 0; i < maxWindows && since.Before(horizon); i++ {
		until := since.Add(w.window())
		if until.After(horizon) {
			until = horizon
		}
		if !until.After(since) {
			break
		}
		progress.Mark(ctx, fmt.Sprintf("refresh %s window %s..%s", provider, since.Format(time.RFC3339), until.Format(time.RFC3339)))
		params := reconcile.RunParams{
			Mode:        mode,
			Mutations:   mutations,
			Providers:   []reconcile.Provider{provider},
			PSPs:        bindings,
			PSPCoverage: coverage,
			Since:       since,
			Until:       until,
		}
		res, err := engine.Run(ctx, params)
		if err != nil {
			out.ProviderErrors++
			// The unadvanced watermark is the durable record that this window
			// still needs reading; the job's log and error report the failure.
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"provider": provider,
				"since":    since,
				"until":    until,
			}).Warn("Provider Refresh: provider event window failed; watermark unchanged")
			break
		}
		out.Windows++
		if rep := res.Summary.Providers[string(provider)]; rep != nil {
			out.NewFindings += rep.NewFindings
			out.UpdatedFindings += rep.UpdatedFindings
		}
		// A completed enforce window's coverage is a pull proof.
		out.Proofs = res.PullProofs()
		out.AppliedChanges += len(res.AppliedChanges)
		out.Changed = out.Changed || len(res.AppliedChanges) > 0
		// Webhook drift: pull-applied corrections while the rail's
		// accepted-webhook watermark predates the previous pull (gated in SQL)
		// are changes a webhook should have announced. Best-effort telemetry.
		if n := len(res.AppliedChanges); n > 0 {
			if _, derr := webhookhealth.Drift(ctx, w.DB, pspID, now, n); derr != nil {
				log.WithContext(ctx).WithError(derr).WithField("provider", provider).Warn("Provider Refresh: record webhook drift failed")
			}
		}
		if err := w.recordWatermarkSuccess(ctx, mid, provider, pspID, until); err != nil {
			out.WatermarkErrors++
			log.WithContext(ctx).WithError(err).WithField("provider", provider).Warn("Provider Refresh: advance watermark failed")
			break
		}
		if qualifiedRecovery {
			if !res.AppliedEventCoverage(provider, since, until) {
				out.ProviderErrors++
				break
			}
			if err := w.DB.Gen(ctx).UpsertPSPAppliedRefreshWatermark(ctx, gen.UpsertPSPAppliedRefreshWatermarkParams{MerchantID: mid, PspID: pspID, WatermarkAt: until}); err != nil {
				out.WatermarkErrors++
				break
			}
		}
		since = until
	}
	out.More = since.Before(horizon) && out.ProviderErrors == 0 && out.WatermarkErrors == 0
	if qualifiedRecovery && !out.More && out.ProviderErrors == 0 && out.WatermarkErrors == 0 {
		conflicts, err := w.DB.Gen(ctx).PSPHasUnresolvedFinancialFindings(ctx, gen.PSPHasUnresolvedFinancialFindingsParams{MerchantID: mid, PspID: pspID})
		if err != nil || conflicts {
			out.ProviderErrors++
		} else {
			more, err := w.completeRecovery(ctx, mid, pspID, horizon)
			if err != nil {
				out.WatermarkErrors++
				log.WithContext(ctx).WithError(err).WithField("psp_id", pspID).Warn("Provider Refresh: completion and recovery wakeups failed")
			} else {
				out.More = more
			}
		}
	}
	// Advance the pull watermark after the pass so the next pass's drift gate
	// compares against this pull.
	if out.Windows > 0 {
		if err := webhookhealth.StampPull(ctx, w.DB, pspID, now); err != nil {
			log.WithContext(ctx).WithError(err).WithField("provider", provider).Warn("Provider Refresh: stamp webhook pull watermark failed")
		}
	}
	return out
}

// Complete coverage and its recovery wakeups commit together. Scans use the
// existing collection policy/cadence; refresh never invokes a provider writer.
func (w *ProviderRefreshWorker) completeRecovery(ctx context.Context, mid, psp uuid.UUID, horizon time.Time) (bool, error) {
	const batch = 100
	more := false
	err := w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := w.DB.NewWithPgxTx(tx)
		changed, err := d.Gen(ctx).UpsertPSPCompletedRefreshWatermark(ctx, gen.UpsertPSPCompletedRefreshWatermarkParams{MerchantID: mid, PspID: psp, WatermarkAt: horizon})
		if err != nil {
			return err
		}
		if (writeposture.View{Config: w.Config, DB: d}).Posture(ctx, mid).Limited() {
			return nil
		}
		if err := providerrecovery.CheckPSP(ctx, d, mid, psp, w.now()); err != nil {
			return err
		}
		resumed, err := intents.NewStore(d).ResumeRecoveryHeld(ctx, w.now(), batch)
		if err != nil {
			return err
		}
		more = resumed == batch
		if changed == 0 && resumed == 0 {
			return nil
		}
		merchantID := billing.MerchantID(mid)
		jobs := []river.JobArgs{DunningArgs{MerchantID: mid}, InvoiceArgs{MerchantID: &merchantID, Collect: true}, InvoiceArgs{MerchantID: &merchantID, Collect: true, UseMonthlyFloor: true}}
		for _, args := range jobs {
			// Do not deduplicate against a scan sleeping on an earlier outage.
			// Canonical invoice/renewal admission and durable cadence own money.
			if err := w.DB.InsertRiverJobTx(ctx, tx, args, &river.InsertOpts{Queue: QueueBilling}); err != nil {
				return err
			}
		}
		return nil
	})
	return more, err
}

func (w *ProviderRefreshWorker) loadAppliedWatermark(ctx context.Context, mid, psp uuid.UUID) (time.Time, error) {
	watermark, err := w.DB.Gen(ctx).GetPSPAppliedRefreshWatermark(ctx, gen.GetPSPAppliedRefreshWatermarkParams{MerchantID: mid, PspID: psp})
	if err == nil {
		if watermark.After(w.now().Add(w.safetyLag())) {
			return time.Time{}, fmt.Errorf("account %s has future-dated applied progress", psp)
		}
		return watermark.UTC(), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, err
	}
	floor, err := w.DB.Gen(ctx).PSPRecoveryHistoryFloor(ctx, gen.PSPRecoveryHistoryFloorParams{MerchantID: mid, PspID: psp, Fallback: w.now().Add(-w.safetyLag())})
	if err != nil {
		return time.Time{}, err
	}

	return floor.UTC(), nil
}

func (w *ProviderRefreshWorker) recordWatermarkSuccess(ctx context.Context, mid uuid.UUID, provider reconcile.Provider, pspID uuid.UUID, watermark time.Time) error {
	return w.DB.Gen(ctx).UpsertPSPRefreshWatermark(ctx, gen.UpsertPSPRefreshWatermarkParams{MerchantID: mid, PspID: pspID, WatermarkAt: watermark.UTC()})
}

func (w *ProviderRefreshWorker) window() time.Duration {
	if w.Window > 0 {
		return w.Window
	}
	return defaultRefreshWindow
}

func (w *ProviderRefreshWorker) safetyLag() time.Duration {
	if w.SafetyLag > 0 {
		return w.SafetyLag
	}
	return defaultRefreshSafetyLag
}

func (w *ProviderRefreshWorker) maxWindows() int {
	if w.MaxWindows > 0 {
		return w.MaxWindows
	}
	return defaultRefreshMaxWindows
}

type providerRefreshStats struct {
	More            bool
	Merchants       int
	Providers       int
	Windows         int
	AppliedChanges  int
	NewFindings     int
	UpdatedFindings int
	WatermarkErrors int
	ProviderErrors  int
	CCBillErrors    int
	ConvergeErrors  int
	LaneErrors      int
	// Gated counts passes that observed receipts with lifecycle changes held.
	Gated int
}

func (s *providerRefreshStats) add(r providerRefreshMerchantResult) {
	s.More = s.More || r.More
	s.Providers += r.Providers
	s.Windows += r.Windows
	s.AppliedChanges += r.AppliedChanges
	s.NewFindings += r.NewFindings
	s.UpdatedFindings += r.UpdatedFindings
	s.WatermarkErrors += r.WatermarkErrors
	s.ProviderErrors += r.ProviderErrors
}

type providerRefreshMerchantResult struct {
	providerRefreshProviderResult
}

func (r *providerRefreshMerchantResult) add(p providerRefreshProviderResult) {
	r.More = r.More || p.More
	r.Providers += p.Providers
	r.Windows += p.Windows
	r.AppliedChanges += p.AppliedChanges
	r.NewFindings += p.NewFindings
	r.UpdatedFindings += p.UpdatedFindings
	r.WatermarkErrors += p.WatermarkErrors
	r.ProviderErrors += p.ProviderErrors
	r.Changed = r.Changed || p.Changed
	if len(p.Proofs) > 0 {
		if r.Proofs == nil {
			r.Proofs = reconcile.PullProofs{}
		}
		r.Proofs.Merge(p.Proofs)
	}
}

type providerRefreshProviderResult struct {
	More            bool
	Providers       int
	Windows         int
	AppliedChanges  int
	NewFindings     int
	UpdatedFindings int
	WatermarkErrors int
	ProviderErrors  int
	Changed         bool
	// Proofs carries the completed pull's coverage for the confirmed-absence gate.
	Proofs reconcile.PullProofs
}

func (w *ProviderRefreshWorker) lockMerchant(ctx context.Context, mid uuid.UUID) (release func(), held bool, err error) {
	pool := w.DB.Pool()
	if pool == nil {
		return nil, false, fmt.Errorf("provider refresh: pool not configured")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	q := gen.New(conn)
	if held, err = q.TryLockProviderRefresh(ctx, mid); err != nil || !held {
		conn.Release()
		return nil, false, err
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := q.UnlockProviderRefresh(unlockCtx, mid); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
		conn.Release()
	}, true, nil
}
