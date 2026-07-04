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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	KindProviderRefresh         = "openrails.provider_refresh"
	KindProviderRefreshMerchant = "openrails.provider_refresh_merchant"

	// QueueProviderRefresh bounds refresh concurrency in standalone (#719): a
	// small MaxWorkers cap is the global brake on refresh HTTP so thousands of
	// merchants can't stampede a provider's API budget. Embedded hosts route
	// the kind onto QueueBilling instead (see AddBillingWorkersTo).
	QueueProviderRefresh = "provider_refresh"

	providerRefreshDomainEvents   = "events"
	defaultRefreshWindow          = 24 * time.Hour
	defaultRefreshSafetyLag       = 5 * time.Minute
	defaultRefreshInitialLookback = 90 * 24 * time.Hour
	defaultRefreshMaxWindows      = 8
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

// refreshJobInserter is the slice of river.Client the scheduler uses (test seam).
type refreshJobInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// ProviderRefreshSchedulerWorker (#719) is the periodic umbrella: it lists
// active merchants and enqueues ONE per-merchant refresh job each, spread
// evenly over Stagger. Merchants that cannot possibly arm (no declared
// rail accounts AND no boot-config fallback plane) are skipped before enqueue
// via a cheap RLS-scoped EXISTS. River supplies the rest: per-merchant error
// isolation + retries, and the bounded queue caps refresh concurrency.
type ProviderRefreshSchedulerWorker struct {
	river.WorkerDefaults[ProviderRefreshArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock

	// Boot-plane deps, presence only: any boot-config fallback rail can arm
	// EVERY merchant (merchant_wiring.go precedence), so the zero-accounts
	// skip applies only when the deployment has no boot plane at all (hosted
	// SaaS posture).
	Rails          config.RailMerchantAccountSet
	NMIClients     map[string]*nmi.NMIClient
	CCBillDataLink *ccbill.DataLinkClient
	SolanaRPC      *solanaint.RPCClient

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

func (w *ProviderRefreshSchedulerWorker) bootPlaneArmed() bool {
	return len(w.Rails) > 0 || len(w.NMIClients) > 0 || w.CCBillDataLink != nil || w.SolanaRPC != nil
}

func (w *ProviderRefreshSchedulerWorker) listMerchants(ctx context.Context) ([]uuid.UUID, error) {
	if w.ListMerchants != nil {
		return w.ListMerchants(ctx)
	}
	return w.DB.Gen(ctx).ListActiveMerchantIDs(ctx)
}

// merchantHasRailAccounts: cheap accounts-exist predicate. rail_merchant_accounts
// is RLS-isolated, so the EXISTS runs under the merchant GUC. Archived rows
// count — drain pulls still arm (#655). Environment is NOT filtered: a
// wrong-environment row enqueues a job that arms nothing (fail open, cheap).
func (w *ProviderRefreshSchedulerWorker) merchantHasRailAccounts(ctx context.Context, mid uuid.UUID) (bool, error) {
	if w.HasRailAccounts != nil {
		return w.HasRailAccounts(ctx, mid)
	}
	var exists bool
	mctx := merchant.WithID(ctx, merchant.ID(mid))
	err := w.DB.MerchantTx(mctx, func(tctx context.Context, tx pgx.Tx) error {
		// Explicit merchant_id predicate: redundant under RLS, load-bearing on
		// BYPASSRLS roles (tests, superuser self-hosts).
		return tx.QueryRow(tctx, `SELECT EXISTS (SELECT 1 FROM openrails.rail_merchant_accounts WHERE merchant_id = $1)`, mid).Scan(&exists)
	})
	return exists, err
}

func (w *ProviderRefreshSchedulerWorker) Work(ctx context.Context, _ *river.Job[ProviderRefreshArgs]) error {
	if w.Config != nil && w.Config.IsProviderReadOnly() {
		log.WithContext(ctx).Warn("Readonly mode: provider refresh scheduling skipped (pure observer; no local convergence)")
		return nil
	}
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
	rand.Shuffle(len(merchantIDs), func(i, j int) { merchantIDs[i], merchantIDs[j] = merchantIDs[j], merchantIDs[i] })

	now := w.now()
	var spacing time.Duration
	if n := len(merchantIDs); n > 0 {
		spacing = w.stagger() / time.Duration(n)
	}
	checkAccounts := !w.bootPlaneArmed()
	var enqueued, deduped, skipped, failed, slot int
	for _, mid := range merchantIDs {
		if checkAccounts {
			ok, err := w.merchantHasRailAccounts(ctx, mid)
			if err != nil {
				// Fail open: a broken predicate must not starve refresh.
				log.WithContext(ctx).WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: accounts predicate failed; enqueueing anyway")
			} else if !ok {
				skipped++
				continue
			}
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
	river.WorkerDefaults[ProviderRefreshMerchantArgs]
	DB     *db.DB
	Config *config.Config
	Rails  config.RailMerchantAccountSet
	Clock  clockwork.Clock
	// Merchants resolves per-merchant provider accounts + scoped secrets
	// (#699). nil = boot-config plane only.
	Merchants           *merchants.Service
	NMIClients          map[string]*nmi.NMIClient
	CCBillDataLink      *ccbill.DataLinkClient
	SolanaRPC           *solanaint.RPCClient
	DeferDelete         subscriptions.DeferredDeleteScheduler
	NotificationService *subscriptions.NotificationService

	// PullEndpoints overrides provider base URLs on store-armed clients
	// (fake-provider test seam).
	PullEndpoints reconcile.ProviderEndpoints

	Window          time.Duration
	SafetyLag       time.Duration
	InitialLookback time.Duration
	MaxWindows      int
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
	if w.Config != nil && w.Config.IsProviderReadOnly() {
		log.WithContext(ctx).Warn("Readonly mode: provider refresh skipped (pure observer; no local convergence)")
		return nil
	}
	mid := job.Args.MerchantID
	if mid == uuid.Nil {
		return fmt.Errorf("provider refresh: merchant_id required")
	}

	logger := log.WithContext(ctx).WithField("worker", KindProviderRefreshMerchant).WithField("merchant_id", mid)
	stats := providerRefreshStats{Merchants: 1}

	// #699: fetchers + per-sub probers (#665) arm PER MERCHANT inside the
	// merchant scope — merchant-store credentials first, boot-config rails as
	// the fallback plane. A rail that cannot arm is absent for that merchant
	// (its WARN names merchant/rail/secret); the other rails keep pulling.
	builder := reconcile.MerchantFetcherBuilder{
		Config:         w.Config,
		Rails:          w.Rails,
		Merchants:      w.Merchants,
		DB:             w.DB,
		NMIClients:     w.NMIClients,
		CCBillDataLink: w.CCBillDataLink,
		SolanaRPC:      w.SolanaRPC,
		Endpoints:      w.PullEndpoints,
	}

	err := w.refreshMerchant(ctx, mid, builder, &stats, logger)

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
	}).Info("Provider Refresh: pass completed")
	if err != nil {
		// Merchant connection failed — nothing ran; river retries with backoff.
		// Lane failures stay best-effort (watermarks resume them next tick).
		return fmt.Errorf("provider refresh: merchant %s: %w", mid, err)
	}
	return nil
}

// refreshMerchant is the pre-#719 per-merchant loop body, verbatim: arm →
// CCBill DataLink lane → event refresh → proofs → scoped convergence if
// changed → unknown-cohort reconcile.
func (w *ProviderRefreshWorker) refreshMerchant(ctx context.Context, mid uuid.UUID, builder reconcile.MerchantFetcherBuilder, stats *providerRefreshStats, logger *log.Entry) error {
	mctx := merchant.WithID(ctx, merchant.ID(mid))
	if err := w.DB.RunInMerchantConn(mctx, func(tctx context.Context) error {
		armed := builder.Build(tctx, merchant.ID(mid))
		if err := w.runCCBillDataLinkLane(tctx, armed.CCBillDataLink); err != nil {
			stats.CCBillErrors++
			logger.WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: CCBill DataLink lane failed")
		}
		res := w.runEventRefresh(tctx, mid, armed.Fetchers)
		stats.add(res)
		// #665 §3.2 confirmed-absence gate: a completed exhaustive pull
		// PROVES a source domain — flip reconciliation_state before the
		// convergence pass so held EXCESS repairs can proceed.
		if len(res.Proofs) > 0 {
			if flipped, err := reconcile.MarkReconciledSourceDomains(tctx, w.DB.Gen(tctx), mid, res.Proofs, w.now()); err != nil {
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
	worker := CCBillReconcileWorker{
		DB:                  w.DB,
		DataLink:            dataLink,
		NotificationService: w.NotificationService,
	}
	return worker.Work(ctx, &river.Job[CCBillReconcileArgs]{})
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
	if w.DeferDelete != nil {
		// #679: a stale-decline cancel must durably queue the deferred NMI
		// delete; without this the lifecycle WARNs and the remote keeps retrying.
		lc.SetDeferredDeleteScheduler(w.DeferDelete)
	}
	res, err := reconcile.ReconcileUnknownCohort(ctx, w.DB, lc, fetchers, probers, merchant.ID(mid), w.now(), reconcile.UnknownReconcileOptions{})
	if res.Renewed+res.Adopted+res.PastDue+res.Cancelled+res.Backfilled > 0 || len(res.RailErrors) > 0 {
		log.WithContext(ctx).WithFields(log.Fields{
			"merchant_id": mid, "renewed": res.Renewed, "adopted": res.Adopted, "past_due": res.PastDue,
			"cancelled": res.Cancelled, "still_unknown": res.StillUnknown, "probed": res.Probed,
			"backfilled": res.Backfilled, "rail_customer_accounts": res.RailCustomers, "rail_errors": len(res.RailErrors),
		}).Info("Provider Refresh: unknown-cohort reconcile")
	}
	return err
}

func (w *ProviderRefreshWorker) runConvergence(ctx context.Context, mid uuid.UUID) error {
	engine := converge.NewConvergeEngine(w.DB)
	engine.Now = func() time.Time { return w.now() }
	_, err := engine.Converge(ctx, converge.Scope{Merchant: merchant.ID(mid)})
	return err
}

func (w *ProviderRefreshWorker) runEventRefresh(ctx context.Context, mid uuid.UUID, fetchers map[reconcile.Provider]reconcile.RailFetcher) providerRefreshMerchantResult {
	result := providerRefreshMerchantResult{}
	providers := refreshProviders(fetchers)
	for _, provider := range providers {
		// Runtime provider-account identity resolution was removed (#592):
		// watermarks key globally per merchant+provider and reconcile runs
		// account-agnostic (provider_accounts is an operator-declared catalog).
		providerRes := w.runProviderEventWindows(ctx, mid, provider, nil, nil, fetchers)
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

func (w *ProviderRefreshWorker) runProviderEventWindows(ctx context.Context, mid uuid.UUID, provider reconcile.Provider, accountID *uuid.UUID, bindings map[reconcile.Provider]reconcile.RailMerchantAccountBinding, fetchers map[reconcile.Provider]reconcile.RailFetcher) providerRefreshProviderResult {
	out := providerRefreshProviderResult{Providers: 1}
	now := w.now()
	horizon := now.Add(-w.safetyLag())
	if !horizon.After(time.Time{}) {
		return out
	}

	since, err := w.loadWatermark(ctx, mid, provider, accountID, horizon.Add(-w.initialLookback()))
	if err != nil {
		out.WatermarkErrors++
		log.WithContext(ctx).WithError(err).WithField("provider", provider).Warn("Provider Refresh: load watermark failed")
		return out
	}
	if !since.Before(horizon) {
		return out
	}

	engine := reconcile.NewEngine(w.DB, w.Config, fetchers)
	engine.Now = func() time.Time { return now }
	if w.DeferDelete != nil {
		// #679: a decider stale-decline cancel from the pull path must durably
		// queue the deferred NMI delete, same as unknown-resolution.
		engine.Decisions = reconcile.NewDecisionApplier(w.DB, w.DeferDelete)
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
		res, err := engine.Run(ctx, reconcile.RunParams{
			Mode:                 reconcile.ModeEnforce,
			Mutations:            &reconcile.LocalMutationPolicy{Insert: true, Overwrite: true},
			Providers:            []reconcile.Provider{provider},
			RailMerchantAccounts: bindings,
			Since:                since,
			Until:                until,
		})
		if err != nil {
			out.ProviderErrors++
			if werr := w.recordWatermarkFailure(ctx, mid, provider, accountID, since, now, err); werr != nil {
				out.WatermarkErrors++
				log.WithContext(ctx).WithError(werr).WithField("provider", provider).Warn("Provider Refresh: record failed provider attempt failed")
			}
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
		if err := w.recordWatermarkSuccess(ctx, mid, provider, accountID, until, now); err != nil {
			out.WatermarkErrors++
			log.WithContext(ctx).WithError(err).WithField("provider", provider).Warn("Provider Refresh: advance watermark failed")
			break
		}
		since = until
	}
	return out
}

func (w *ProviderRefreshWorker) loadWatermark(ctx context.Context, mid uuid.UUID, provider reconcile.Provider, accountID *uuid.UUID, fallback time.Time) (time.Time, error) {
	var watermark time.Time
	err := w.DB.Qx(ctx).QueryRow(ctx, `
SELECT watermark_at
  FROM openrails.rail_refresh_watermarks
 WHERE merchant_id = $1::uuid
   AND rail = $2::text
   AND event_domain = $3::text
   AND rail_merchant_account_key = COALESCE($4::uuid, '00000000-0000-0000-0000-000000000000'::uuid)
`, mid, string(provider), providerRefreshDomainEvents, accountID).Scan(&watermark)
	if errors.Is(err, pgx.ErrNoRows) {
		return fallback.UTC(), nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return watermark.UTC(), nil
}

func (w *ProviderRefreshWorker) recordWatermarkSuccess(ctx context.Context, mid uuid.UUID, provider reconcile.Provider, accountID *uuid.UUID, watermark, attemptedAt time.Time) error {
	_, err := w.DB.Qx(ctx).Exec(ctx, `
INSERT INTO openrails.rail_refresh_watermarks (
    merchant_id, rail, rail_merchant_account_id, event_domain, watermark_at,
    last_attempted_at, last_succeeded_at, last_error
) VALUES ($1::uuid, $2::text, $3::uuid, $4::text, $5::timestamptz, $6::timestamptz, $6::timestamptz, NULL)
ON CONFLICT ON CONSTRAINT rail_refresh_watermarks_identity_key
DO UPDATE SET
    watermark_at = EXCLUDED.watermark_at,
    last_attempted_at = EXCLUDED.last_attempted_at,
    last_succeeded_at = EXCLUDED.last_succeeded_at,
    last_error = NULL,
    updated_at = now()
`, mid, string(provider), accountID, providerRefreshDomainEvents, watermark.UTC(), attemptedAt.UTC())
	return err
}

func (w *ProviderRefreshWorker) recordWatermarkFailure(ctx context.Context, mid uuid.UUID, provider reconcile.Provider, accountID *uuid.UUID, watermark, attemptedAt time.Time, cause error) error {
	errText := cause.Error()
	_, err := w.DB.Qx(ctx).Exec(ctx, `
INSERT INTO openrails.rail_refresh_watermarks (
    merchant_id, rail, rail_merchant_account_id, event_domain, watermark_at,
    last_attempted_at, last_succeeded_at, last_error
) VALUES ($1::uuid, $2::text, $3::uuid, $4::text, $5::timestamptz, $6::timestamptz, NULL, $7::text)
ON CONFLICT ON CONSTRAINT rail_refresh_watermarks_identity_key
DO UPDATE SET
    last_attempted_at = EXCLUDED.last_attempted_at,
    last_error = EXCLUDED.last_error,
    updated_at = now()
`, mid, string(provider), accountID, providerRefreshDomainEvents, watermark.UTC(), attemptedAt.UTC(), errText)
	return err
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

func (w *ProviderRefreshWorker) initialLookback() time.Duration {
	if w.InitialLookback > 0 {
		return w.InitialLookback
	}
	return defaultRefreshInitialLookback
}

func (w *ProviderRefreshWorker) maxWindows() int {
	if w.MaxWindows > 0 {
		return w.MaxWindows
	}
	return defaultRefreshMaxWindows
}

type providerRefreshStats struct {
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
}

func (s *providerRefreshStats) add(r providerRefreshMerchantResult) {
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
