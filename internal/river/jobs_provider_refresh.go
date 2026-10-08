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
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhookhealth"
	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/progress"
)

const (
	KindProviderRefresh         = "openrails.provider_refresh"
	KindProviderRefreshMerchant = "openrails.provider_refresh_merchant"

	// QueueProviderRefresh bounds refresh concurrency in standalone (#719): a
	// small MaxWorkers cap is the global brake on refresh HTTP so thousands of
	// merchants can't stampede a provider's API budget. Embedded hosts route
	// the kind onto QueueBilling instead (see AddBillingWorkersTo).
	QueueProviderRefresh = "provider_refresh"

	providerRefreshDomainEvents = "events"
	defaultRefreshWindow        = 24 * time.Hour
	defaultRefreshSafetyLag     = 5 * time.Minute
	defaultRefreshMaxWindows    = 8
	// defaultRefreshStagger spreads the scheduler's fan-out so merchant pull
	// windows don't align on the tick instant.
	defaultRefreshStagger = 30 * time.Minute
)

// ProviderRefreshArgs is the periodic SCHEDULER kind (#719). The kind string is
// unchanged from the pre-#719 serial umbrella, so a pending umbrella row from a
// mid-upgrade deployment is worked as a scheduler pass (fan-out), never as a
// second serial loop.
type ProviderRefreshArgs struct{}

func (ProviderRefreshArgs) Kind() string { return KindProviderRefresh }

// ProviderRefreshMerchantArgs is one merchant's refresh job (#719).
type ProviderRefreshMerchantArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	// Requested marks a host-requested refresh: it must observe provider state
	// from after the request, so it never merges into a scheduled pass that
	// may already be running.
	Requested bool `json:"requested,omitempty" river:"unique"`
}

func (ProviderRefreshMerchantArgs) Kind() string { return KindProviderRefreshMerchant }

// providerRefreshUniqueStates = default unique set minus completed: exactly one
// IN-FLIGHT job per merchant, re-enqueueable next tick. Overlapping scheduler
// passes (a mid-upgrade leftover umbrella row next to the fresh periodic tick)
// dedupe here instead of double-refreshing a merchant.
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

// ProviderRefreshSchedulerWorker (#719) is the periodic umbrella: it lists
// active merchants and enqueues ONE per-merchant refresh job each, spread
// evenly over Stagger. Merchants that cannot possibly arm (no declared
// rail accounts AND no boot-config fallback plane) are skipped before enqueue
// via a cheap merchant-scoped EXISTS. River supplies the rest: per-merchant error
// isolation + retries, and the bounded queue caps refresh concurrency.
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

// merchantHasRailAccounts: cheap accounts-exist predicate. psps
// is merchant-owned, so the EXISTS runs per merchant. Archived rows
// count — drain pulls still arm (#655). Environment is NOT filtered: a
// wrong-environment row enqueues a job that arms nothing (fail open, cheap).
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
	// #788: the armed rail state (psps) is the only
	// credential plane, so a merchant with zero declared accounts has nothing
	// to refresh — always skip it before enqueue.
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
		"stagger":             w.stagger().String(),
	}).Info("Provider Refresh: scheduled merchant refresh jobs")
	if failed > 0 {
		// Retry the scheduler; per-merchant unique keys make the rerun idempotent.
		return fmt.Errorf("provider refresh scheduler: %d/%d enqueues failed", failed, len(merchantIDs))
	}
	return nil
}

// ProviderRefreshWorker (#574) is ONE merchant's provider-read refresh (#719:
// the kind fans out from the scheduler above). It keeps provider truth fresh
// without remote mutations: bounded event pulls with durable watermarks, the
// unknown-cohort reconcile (#632/#633/#665 — the ONE per-subscription
// verification path), and the CCBill DataLink bulk lane. Provider outages
// leave watermarks in place for the next scheduled/startup pass.
//
// Credentials arm PER MERCHANT (#699): fetchers/probers are built inside the
// merchant's scope from the merchant-secrets store first, with the boot-config
// rails (Rails + the boot-built clients below) as the fallback plane. Clients
// are cheap per-merchant structs — nothing credentialed is cached across
// merchants (#653).
type ProviderRefreshWorker struct {
	StripeClients *stripeapi.Factory
	river.WorkerDefaults[ProviderRefreshMerchantArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	// Merchants resolves per-merchant PSPs + scoped secrets
	// (#699/#788 — the ONLY credential plane). nil = nothing arms.
	Merchants   *merchants.Service
	DeferDelete subscriptions.ProviderCancelScheduler
	// Alerts bridges requires_review findings into the #736 operator
	// notification store (#787). nil = no-op (no alerting service wired).
	Alerts *alerting.Service

	// PullEndpoints overrides provider base URLs on store-armed clients
	// (fake-provider test seam).
	PullEndpoints reconcile.ProviderEndpoints
	NMIClients    *railresolve.NMIFactory
	// Verifier reads the unverified NMI rows (#1094); nil leaves them to the
	// unknown-cohort reconcile.
	Verifier *reconcile.Verifier

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

	// #699/#788: fetchers + per-sub probers (#665) arm PER MERCHANT inside
	// the merchant scope from the armed rail state. A rail that cannot arm is
	// absent for that merchant (its WARN names merchant/rail/secret); the
	// other rails keep pulling.
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
	if stats.ProviderErrors+stats.WatermarkErrors+stats.LaneErrors+stats.ConvergeErrors+stats.CCBillErrors > 0 {
		return fmt.Errorf("provider refresh incomplete: provider=%d watermark=%d lane=%d", stats.ProviderErrors, stats.WatermarkErrors, stats.LaneErrors)
	}
	if stats.More {
		return river.JobSnooze(time.Second)
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
		overwrite := verdict.Allowed && verdict.EnforceArmed && (w.Config == nil || !config.IsProviderReadOnly(w.Config))
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
				stats.ProviderErrors++
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

		// #665 §3.2 confirmed-absence gate: a completed exhaustive pull
		// PROVES a source domain — flip reconciliation_state before the
		// convergence pass so held EXCESS repairs can proceed.
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
		// #633: reconcile the `unknown` cohort against provider truth (one
		// windowed bulk pull per rail + targeted per-sub probe fallbacks).
		// Tolerant of missing/failed fetchers — those rails' subs stay
		// `unknown` and are retried next pass (backoff).
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

// runUnknownReconcile resolves the merchant's `unknown` subscription cohort (#632)
// against provider truth via one windowed bulk pull per rail (#633), backfilling
// missing charges (#634). The lifecycle service is built with nil deps (like the
// converge engine): the resolver only needs local-state writes + the entitlement
// service it constructs internally for revokes — plus the deferred-delete
// scheduler (#679) so stale-decline cancels queue the NMI delete intent.
// Best-effort: a rail whose pull fails leaves its subs `unknown` for the next
// scheduled pass (River backoff).
func (w *ProviderRefreshWorker) runUnknownReconcile(ctx context.Context, mid uuid.UUID, fetchers map[reconcile.Provider]reconcile.RailFetcher, probers map[reconcile.Provider]reconcile.SubscriptionProber) error {
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	lc := subscriptions.NewSubscriptionLifecycleService(w.DB, nil, nil, nil, nil, nil, clock)
	lc.SetConfig(w.Config)
	if w.DeferDelete != nil {
		// #679: a stale-decline cancel must durably queue the deferred NMI
		// delete; without this the lifecycle WARNs and the remote keeps retrying.
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
		// #787: nil-check before assigning to the interface field — a nil
		// *alerting.Service boxed into a non-nil FindingNotifier interface
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
		// or#893: the pass already KNOWS which PSP armed the rail's fetcher —
		// resolveScopeCoverage recorded it. It used to be thrown away, so the
		// watermark keyed globally per (merchant, rail) and reconcile ran
		// account-agnostic: pulling mobius advanced paykings' watermark past
		// events nobody had read, and every mirror row landed unattributed.
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

// refreshProviders selects the pull lanes to run. Only what actively matters
// runs: fetchers arm PER MERCHANT from the secrets store (#699) — a merchant
// with no credentials/accounts on a rail gets no fetcher and no pull. The
// allowed map is the second gate: lane production-readiness (solana joined
// once #714/#715 made its fetcher real).
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

	since, err := w.loadAppliedWatermark(ctx, mid, pspID)
	if err != nil {
		out.WatermarkErrors++
		log.WithContext(ctx).WithError(err).WithField("provider", provider).Warn("Provider Refresh: load watermark failed")
		return out
	}
	// Repeat a bounded window for late provider indexing. This is recovery
	// overlap, not a claim that the provider can never publish older history.
	since = since.Add(-w.window())
	if !since.Before(horizon) {
		return out
	}

	engine := reconcile.NewEngine(w.DB, w.Config, fetchers, w.DeferDelete)
	engine.Now = func() time.Time { return now }
	if w.Alerts != nil {
		engine.Notifier = w.Alerts // #787: nil-check, see runConvergence
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
			// or#823: the failure is recorded by the job (log + River's own error
			// record), not by a watermark column nothing surfaced. The row is
			// deliberately left alone: an unadvanced watermark IS the durable
			// statement that this window still needs reading.
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
		// Completed enforce window: its coverage is a pull proof (#665 gate).
		out.Proofs = res.PullProofs()
		out.AppliedChanges += len(res.AppliedChanges)
		out.Changed = out.Changed || len(res.AppliedChanges) > 0
		// #786 drift: pull-applied corrections while the rail's accepted-webhook
		// watermark predates the previous pull (gated in SQL) — changes a
		// webhook should have announced. Best-effort telemetry.
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
		if !res.AppliedEventCoverage(provider, since, until) {
			out.ProviderErrors++
			break
		}
		if err := w.DB.Gen(ctx).UpsertPSPAppliedRefreshWatermark(ctx, gen.UpsertPSPAppliedRefreshWatermarkParams{MerchantID: mid, PspID: pspID, WatermarkAt: until}); err != nil {
			out.WatermarkErrors++
			break
		}
		since = until
	}
	out.More = since.Before(horizon) && out.ProviderErrors == 0 && out.WatermarkErrors == 0
	if !out.More && out.ProviderErrors == 0 && out.WatermarkErrors == 0 {
		conflicts, err := w.DB.Gen(ctx).PSPHasUnresolvedFinancialFindings(ctx, gen.PSPHasUnresolvedFinancialFindingsParams{MerchantID: mid, PspID: pspID})
		if err != nil || conflicts {
			out.ProviderErrors++
		} else if err := w.DB.Gen(ctx).UpsertPSPCompletedRefreshWatermark(ctx, gen.UpsertPSPCompletedRefreshWatermarkParams{MerchantID: mid, PspID: pspID, WatermarkAt: horizon}); err != nil {
			out.WatermarkErrors++
		}
	}
	// #786: advance the pull watermark AFTER the pass so the NEXT pass's drift
	// gate compares against this pull.
	if out.Windows > 0 {
		if err := webhookhealth.StampPull(ctx, w.DB, pspID, now); err != nil {
			log.WithContext(ctx).WithError(err).WithField("provider", provider).Warn("Provider Refresh: stamp webhook pull watermark failed")
		}
	}
	return out
}

func (w *ProviderRefreshWorker) loadAppliedWatermark(ctx context.Context, mid, psp uuid.UUID) (time.Time, error) {
	watermark, err := w.DB.Gen(ctx).GetPSPAppliedRefreshWatermark(ctx, gen.GetPSPAppliedRefreshWatermarkParams{MerchantID: mid, PspID: psp})
	if err == nil {
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
	// Proofs carries the completed pull's coverage for the #665 gate.
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
