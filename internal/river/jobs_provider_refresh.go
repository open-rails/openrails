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
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhookhealth"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/cadence"
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
	defaultRefreshOverlap       = 24 * time.Hour
	defaultRefreshSafetyLag     = 5 * time.Minute
	defaultRefreshBatch         = 8 * 24 * time.Hour
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

// refreshJobInserter is the slice of river.Client the scheduler uses (test seam).
type refreshJobInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// ProviderRefreshSchedulerWorker is the periodic umbrella: it enqueues one
// refresh job per active merchant that has a PSP, spread evenly over Stagger.
// River supplies per-merchant error isolation and retries; the bounded queue
// caps refresh concurrency.
type ProviderRefreshSchedulerWorker struct {
	WorkerDefaults[ProviderRefreshArgs]
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
	WorkerDefaults[ProviderRefreshMerchantArgs]
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
	// Positive usage-order recovery does not authorize provider or lifecycle writes.
	RecoverUsagePayment func(context.Context, uuid.UUID, string) error

	// Overlap is re-read for late provider indexing; Batch bounds the span
	// of one read, the rest continuing in the next job.
	Overlap   time.Duration
	SafetyLag time.Duration
	Batch     time.Duration
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
	release, held, err := reconcile.LockMerchantPull(ctx, w.DB.Pool(), mid)
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
		"converge_errors":  stats.ConvergeErrors,
		"lane_errors":      stats.LaneErrors,
		"gated":            stats.Gated,
		"held":             stats.Held,
		"unarmed":          stats.Unarmed,
	}).Info("Provider Refresh: pass completed")
	if err != nil {
		// Merchant connection failed — nothing ran; river retries with backoff.
		// Lane failures stay best-effort (watermarks resume them next tick).
		return fmt.Errorf("provider refresh: merchant %s: %w", mid, err)
	}
	if stats.More {
		return river.JobSnooze(time.Second)
	}
	if stats.ProviderErrors+stats.WatermarkErrors+stats.LaneErrors+stats.ConvergeErrors > 0 {
		return fmt.Errorf("provider refresh incomplete: provider=%d watermark=%d lane=%d converge=%d",
			stats.ProviderErrors, stats.WatermarkErrors, stats.LaneErrors, stats.ConvergeErrors)
	}
	if stats.Held > 0 && stats.NewFindings > 0 {
		// Findings this pass raised may be stale: a concurrent verifier or
		// webhook can record the receipt after the read. One more pass tells
		// them from findings that stand (it raises none new).
		return river.JobSnooze(time.Second)
	}
	if stats.Held > 0 {
		// Every read completed. Unresolved financial observations remain in
		// the findings list for review; they do not pause ordinary billing.
		logger.WithField("psps_with_unresolved_findings", stats.Held).Warn("Provider Refresh: unresolved financial observations need review")
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
		held, enforceArmed := reconcile.OverwriteHeld(tctx, w.DB, w.Config, mid)
		overwrite := held == "" && enforceArmed
		if !overwrite {
			stats.Gated++
		}
		accounts, err := w.DB.Gen(tctx).ListPSPsForMerchant(tctx, mid)
		if err != nil {
			return err
		}
		var res providerRefreshMerchantResult
		for _, account := range accounts {
			provider := reconcile.Provider(account.Rail)
			accountBuilder := builder
			accountBuilder.AccountIDs = map[reconcile.Provider]string{provider: account.AccountID}
			armed := accountBuilder.Build(tctx, billing.MerchantID(mid))
			fetcher, ok := armed.Fetchers[provider]
			switch {
			case !ok && armed.Unavailable[provider]:
				// Configuration unreadable is not "not armed": read it again.
				stats.ProviderErrors++
				continue
			case !ok:
				// Not armed (its WARN names the missing secret): nothing to read,
				// so nothing incomplete. It joins the first pass after arming.
				stats.Unarmed++
				logger.WithFields(log.Fields{"psp_id": account.ID, "rail": account.Rail}).Debug("Provider Refresh: PSP not armed; not refreshable")
				continue
			case armed.Coverage[provider].Binding.ID != account.ID:
				stats.ProviderErrors++
				continue
			}
			accountOverwrite := overwrite
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
		if !overwrite {
			return nil
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
		// Reconcile the `unknown` cohort against the provider, each row with
		// its own PSP. A PSP that cannot arm leaves its rows `unknown`.
		if err := w.runUnknownReconcile(tctx, mid, builder); err != nil {
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

// runUnknownReconcile resolves the merchant's `unknown` subscriptions against
// the provider (one windowed bulk pull per PSP, probes as fallback),
// backfilling missing charges; with a Verifier, NMI rows go to it instead. The
// lifecycle service needs only local writes, so it is built with nil deps.
// Best-effort: a PSP whose pull fails leaves its subscriptions `unknown` for
// the next pass.
func (w *ProviderRefreshWorker) runUnknownReconcile(ctx context.Context, mid uuid.UUID, builder reconcile.MerchantFetcherBuilder) error {
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
	res, err := reconcile.ReconcileUnknownCohort(ctx, w.DB, lc, builder, billing.MerchantID(mid), w.now(), opts)
	err = errors.Join(verifyErr, err)
	if res.Held > 0 {
		log.WithContext(ctx).WithFields(log.Fields{"merchant_id": mid, "held": res.Held}).
			Warn("Provider Refresh: unknown-cohort cancellations held by the cancel gate (kill switch, arming or budget); the rows stay unverified")
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
	engine := converge.NewConvergeEngine(w.DB, w.Clock)
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
	// Repeat a bounded overlap for late provider indexing. This is recovery
	// overlap, not a claim that the provider can never publish older history.
	if qualifiedRecovery {
		since = since.Add(-w.overlap())
	}
	if !since.Before(horizon) {
		return out
	}

	engine := reconcile.NewEngine(w.DB, w.Config, w.Contacts, fetchers, w.DeferDelete)
	engine.RecoverUsagePayment = w.RecoverUsagePayment
	engine.Now = func() time.Time { return now }
	if w.Alerts != nil {
		engine.Notifier = w.Alerts // nil-check: see runConvergence
	}
	// A run reads the account's whole roster, so one run covers the batch: a
	// run per day would read the roster once per day of backlog.
	until := since.Add(w.batch())
	if until.After(horizon) {
		until = horizon
	}
	params := reconcile.RunParams{
		Mode:        mode,
		Mutations:   mutations,
		Providers:   []reconcile.Provider{provider},
		PSPs:        map[reconcile.Provider]reconcile.PSPBinding{provider: binding},
		PSPCoverage: coverage,
		Since:       since,
		Until:       until,
	}
	applied := func() bool {
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
			return false
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
			return false
		}
		if qualifiedRecovery {
			switch res.AppliedEventCoverage(provider, since, until) {
			case reconcile.CoverageApplied:
			case reconcile.CoverageHeld:
				out.Held++
				return false
			default:
				out.ProviderErrors++
				return false
			}
			if err := w.DB.Gen(ctx).UpsertPSPAppliedRefreshWatermark(ctx, gen.UpsertPSPAppliedRefreshWatermarkParams{MerchantID: mid, PspID: pspID, WatermarkAt: until}); err != nil {
				out.WatermarkErrors++
				return false
			}
		}
		return true
	}()
	if applied {
		since = until
	}
	out.More = since.Before(horizon) && out.ProviderErrors == 0 && out.WatermarkErrors == 0 && out.Held == 0
	if qualifiedRecovery && !out.More && out.ProviderErrors == 0 && out.WatermarkErrors == 0 && out.Held == 0 {
		conflicts, err := w.DB.Gen(ctx).PSPHasUnresolvedFinancialFindings(ctx, gen.PSPHasUnresolvedFinancialFindingsParams{MerchantID: mid, PspID: pspID})
		if err != nil {
			out.WatermarkErrors++
			log.WithContext(ctx).WithError(err).WithField("psp_id", pspID).Warn("Provider Refresh: read open financial findings failed")
		} else if conflicts {
			out.Held++
		} else {
			more, err := w.completeObservation(ctx, mid, pspID, horizon)
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

// Complete coverage records reporting progress; it never grants write permission. Scans use the
// existing collection policy/cadence; refresh never invokes a provider writer.
func (w *ProviderRefreshWorker) completeObservation(ctx context.Context, mid, psp uuid.UUID, horizon time.Time) (bool, error) {
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
		if changed == 0 {
			return nil
		}
		merchantID := billing.MerchantID(mid)
		jobs := []river.JobArgs{DunningArgs{MerchantID: mid}}
		if w.Config != nil && w.Config.UsageBillingEnabled {
			jobs = append(jobs, money.UsageOrderArgs{MerchantID: &merchantID})
		}
		for _, args := range jobs {
			// Do not deduplicate against a scan sleeping on an earlier outage.
			// Canonical usage/renewal admission and durable cadence own money.
			if _, err := w.DB.InsertRiverJobTx(ctx, tx, args, &river.InsertOpts{Queue: QueueBilling}); err != nil {
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

func (w *ProviderRefreshWorker) overlap() time.Duration {
	if w.Overlap > 0 {
		return w.Overlap
	}
	return defaultRefreshOverlap
}

func (w *ProviderRefreshWorker) safetyLag() time.Duration {
	if w.SafetyLag > 0 {
		return w.SafetyLag
	}
	return defaultRefreshSafetyLag
}

func (w *ProviderRefreshWorker) batch() time.Duration {
	if w.Batch > 0 {
		return w.Batch
	}
	return defaultRefreshBatch
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
	ConvergeErrors  int
	LaneErrors      int
	// Gated counts passes that observed receipts with lifecycle changes held.
	Gated int
	// Held counts PSPs read in full whose financial recovery waits on
	// operator-actionable findings; Unarmed, PSPs with no pull credentials.
	// Neither makes the pass incomplete.
	Held    int
	Unarmed int
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
	s.Held += r.Held
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
	r.Held += p.Held
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
	Held            int
	Changed         bool
	// Proofs carries the completed pull's coverage for the confirmed-absence gate.
	Proofs reconcile.PullProofs
}
