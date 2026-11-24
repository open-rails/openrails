package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhookhealth"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
)

const (
	KindProviderRefreshRequested = "openrails.provider_refresh_requested"

	// QueueProviderRefresh bounds provider reads independently of billing work.
	QueueProviderRefresh = "provider_refresh"

	defaultRefreshOverlap   = 24 * time.Hour
	defaultRefreshSafetyLag = 5 * time.Minute
	defaultRefreshBatch     = 8 * 24 * time.Hour
)

// ProviderRefreshRequestArgs is an operator's request to refresh one merchant.
type ProviderRefreshRequestArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
}

func (ProviderRefreshRequestArgs) Kind() string { return KindProviderRefreshRequested }

func (ProviderRefreshRequestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueProviderRefresh,
		MaxAttempts: 5,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: outstanding},
	}
}

// RequestProviderRefresh queues one merchant's refresh, coalescing requests
// while work for that merchant is already outstanding.
func RequestProviderRefresh(ctx context.Context, database *db.DB, id billing.MerchantID) (*rivertype.JobInsertResult, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("provider refresh requires a merchant")
	}
	var requested *rivertype.JobInsertResult
	err := database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		requested, err = database.InsertRiverJobTx(ctx, tx, ProviderRefreshRequestArgs{MerchantID: id.UUID()}, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return requested, nil
}

// ProviderRefreshWorker processes an operator request for one merchant. It
// reads provider events in bounded windows with durable watermarks. Fetchers
// and probers use the merchant's own configured PSP accounts.
type ProviderRefreshWorker struct {
	StripeClients *stripeapi.Factory
	WorkerDefaults[ProviderRefreshRequestArgs]
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

	NMIClients *railresolve.NMIFactory
	// Verifier reads the unverified NMI rows; nil leaves them to the
	// unknown-cohort reconcile.
	Verifier *reconcile.Verifier
	// Positive usage-order recovery does not authorize provider or lifecycle writes.
	RecoverUsagePayment func(context.Context, uuid.UUID, string) error
	// Disputes records the dispute cases each pull reads.
	Disputes reconcile.DisputeRecorder

	// Overlap is re-read for late provider indexing; Batch bounds one window
	// before the same request continues with the next window.
	Overlap   time.Duration
	SafetyLag time.Duration
	Batch     time.Duration
	Log       *slog.Logger
}

func (ProviderRefreshWorker) Kind() string { return KindProviderRefreshRequested }

func (w *ProviderRefreshWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *ProviderRefreshWorker) Work(ctx context.Context, job *river.Job[ProviderRefreshRequestArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("provider refresh: db not configured")
	}
	mid := job.Args.MerchantID
	if mid == uuid.Nil {
		return fmt.Errorf("provider refresh: merchant_id required")
	}

	logger := w.Log.With("worker", KindProviderRefreshRequested, "merchant_id", mid)
	stats := providerRefreshStats{Merchants: 1}

	// One refresh per merchant at a time, across every replica.
	release, held, err := reconcile.LockMerchantPull(ctx, w.DB, mid)
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
		NMIClients:    w.NMIClients,
		Log:           w.Log,
	}

	err = w.refreshMerchant(ctx, mid, builder, &stats, logger)

	logger.InfoContext(ctx, "Provider Refresh: pass completed", "merchants", stats.Merchants, "windows", stats.Windows, "providers", stats.Providers, "applied_changes", stats.AppliedChanges, "new_findings", stats.NewFindings, "updated_findings", stats.UpdatedFindings, "watermark_errors", stats.WatermarkErrors, "provider_errors", stats.ProviderErrors, "converge_errors", stats.ConvergeErrors, "lane_errors", stats.LaneErrors, "gated", stats.Gated, "held", stats.Held, "unarmed", stats.Unarmed)
	if err != nil {
		return fmt.Errorf("provider refresh: merchant %s: %w", mid, err)
	}
	if stats.ProviderErrors+stats.WatermarkErrors+stats.LaneErrors+stats.ConvergeErrors > 0 {
		return fmt.Errorf("provider refresh for %s incomplete: provider errors %d, watermark errors %d, verification errors %d, convergence errors %d",
			mid, stats.ProviderErrors, stats.WatermarkErrors, stats.LaneErrors, stats.ConvergeErrors)
	}
	if stats.More {
		return river.JobSnooze(time.Second)
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
		logger.WarnContext(ctx, "Provider Refresh: unresolved financial observations need review", "psps_with_unresolved_findings", stats.Held)
	}
	return nil
}

// refreshMerchant reads and repairs each account's financial mirror first.
// Policy-held lifecycle repairs remain separate from that observation phase.
func (w *ProviderRefreshWorker) refreshMerchant(ctx context.Context, mid uuid.UUID, builder reconcile.MerchantFetcherBuilder, stats *providerRefreshStats, logger *slog.Logger) error {
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
				logger.DebugContext(ctx, "Provider Refresh: PSP not armed; not refreshable", "psp_id", account.ID, "rail", account.Rail)
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
				logger.WarnContext(ctx, "Provider Refresh: mark reconciled domains failed", logx.Err(err), "merchant_id", mid)
			} else if len(flipped) > 0 {
				logger.InfoContext(ctx, "Provider Refresh: source domains proven reconciled", "merchant_id", mid, "domains", flipped)
			}
		}
		if res.Changed {
			if err := w.runConvergence(tctx, mid); err != nil {
				stats.ConvergeErrors++
				logger.WarnContext(ctx, "Provider Refresh: scoped convergence failed", logx.Err(err), "merchant_id", mid)
			}
		}
		// Reconcile the `unknown` cohort against the provider, each row with
		// its own PSP. A PSP that cannot arm leaves its rows `unknown`.
		if err := w.runUnknownReconcile(tctx, mid, builder); err != nil {
			stats.LaneErrors++
			logger.WarnContext(ctx, "Provider Refresh: unknown-cohort reconcile failed", logx.Err(err), "merchant_id", mid)
		}
		return nil
	}); err != nil {
		stats.LaneErrors++
		logger.ErrorContext(ctx, "Provider Refresh: merchant connection failed", logx.Err(err), "merchant_id", mid)
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
	if len(res.RailErrors) > 0 {
		err = errors.Join(err, fmt.Errorf("verification incomplete for %d provider rails", len(res.RailErrors)))
	}
	if res.Held > 0 {
		w.Log.WarnContext(ctx, "Provider Refresh: unknown-cohort cancellations held by the cancel gate (kill switch, arming or budget); the rows stay unverified", "merchant_id", mid, "held", res.Held)
	}
	if res.Renewed+res.Adopted+res.PastDue+res.Canceled+res.Backfilled > 0 || len(res.RailErrors) > 0 {
		w.Log.InfoContext(ctx, "Provider Refresh: unknown-cohort reconcile", "merchant_id", mid, "renewed", res.Renewed, "adopted", res.Adopted, "past_due", res.PastDue, "canceled", res.Canceled, "still_unknown", res.StillUnknown, "probed", res.Probed, "backfilled", res.Backfilled, "psp_customers", res.RailCustomers, "rail_errors", len(res.RailErrors))
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
			w.Log.ErrorContext(ctx, "Provider Refresh: rail armed without a resolved PSP; refusing an unattributed pull", "merchant_id", mid, "provider", provider)
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
		w.Log.WarnContext(ctx, "Provider Refresh: load watermark failed", logx.Err(err), "provider", provider)
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
	engine.Disputes = w.Disputes
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
		Mode:                    mode,
		Mutations:               mutations,
		Providers:               []reconcile.Provider{provider},
		PSPs:                    map[reconcile.Provider]reconcile.PSPBinding{provider: binding},
		PSPCoverage:             coverage,
		Since:                   since,
		Until:                   until,
		SkipSolanaPlanDiscovery: provider == reconcile.ProviderSolana && until.Before(horizon),
	}
	if provider == reconcile.ProviderStripe {
		w.runReversals(ctx, engine, params, provider, pspID, horizon, now, &out)
	}
	applied := func() bool {
		res, err := engine.Run(ctx, params)
		if err != nil {
			out.ProviderErrors++
			// The unadvanced watermark is the durable record that this window
			// still needs reading; the job's log and error report the failure.
			w.Log.WarnContext(ctx, "Provider Refresh: provider event window failed; watermark unchanged", logx.Err(err), "provider", provider, "since", since, "until", until)
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
				w.Log.WarnContext(ctx, "Provider Refresh: record webhook drift failed", logx.Err(derr), "provider", provider)
			}
		}
		if err := w.recordWatermarkSuccess(ctx, mid, provider, pspID, until); err != nil {
			out.WatermarkErrors++
			w.Log.WarnContext(ctx, "Provider Refresh: advance watermark failed", logx.Err(err), "provider", provider)
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
			w.Log.WarnContext(ctx, "Provider Refresh: read open financial findings failed", logx.Err(err), "psp_id", pspID)
		} else if conflicts {
			out.Held++
		}
	}
	// Advance the pull watermark after the pass so the next pass's drift gate
	// compares against this pull.
	if out.Windows > 0 {
		if err := webhookhealth.StampPull(ctx, w.DB, pspID, now); err != nil {
			w.Log.WarnContext(ctx, "Provider Refresh: stamp webhook pull watermark failed", logx.Err(err), "provider", provider)
		}
	}
	return out
}

// runReversals reads the PSP's refunds and disputes before its full read, which
// a large book may never finish. It moves no watermark: while the full read
// is stuck, every pass re-reads the last batch up to the horizon.
func (w *ProviderRefreshWorker) runReversals(ctx context.Context, engine *reconcile.Engine, params reconcile.RunParams, provider reconcile.Provider, pspID uuid.UUID, horizon, now time.Time, out *providerRefreshProviderResult) {
	params.Reversals = true
	if floor := horizon.Add(-w.batch()); params.Since.Before(floor) {
		params.Since = floor
	}
	params.Until = horizon
	res, err := engine.Run(ctx, params)
	if err != nil {
		out.LaneErrors++
		w.Log.WarnContext(ctx, "Provider Refresh: reversals read failed; the full read still runs", logx.Err(err), "provider", provider)
		return
	}
	if rep := res.Summary.Providers[string(provider)]; rep != nil {
		out.NewFindings += rep.NewFindings
		out.UpdatedFindings += rep.UpdatedFindings
	}
	if n := len(res.AppliedChanges); n > 0 {
		out.AppliedChanges += n
		out.Changed = true
		if _, err := webhookhealth.Drift(ctx, w.DB, pspID, now, n); err != nil {
			w.Log.WarnContext(ctx, "Provider Refresh: record webhook drift failed", logx.Err(err), "provider", provider)
		}
	}
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
	s.LaneErrors += r.LaneErrors
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
	r.LaneErrors += p.LaneErrors
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
	// LaneErrors counts a failed reversals read; the full read still ran.
	LaneErrors int
	Held       int
	Changed    bool
	// Proofs carries the completed pull's coverage for the confirmed-absence gate.
	Proofs reconcile.PullProofs
}
