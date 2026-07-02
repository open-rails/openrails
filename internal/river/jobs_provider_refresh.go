package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	KindProviderRefresh = "openrails.provider_refresh"

	providerRefreshDomainEvents   = "events"
	defaultRefreshWindow          = 24 * time.Hour
	defaultRefreshSafetyLag       = 5 * time.Minute
	defaultRefreshInitialLookback = 90 * 24 * time.Hour
	defaultRefreshMaxWindows      = 8
)

type ProviderRefreshArgs struct{}

func (ProviderRefreshArgs) Kind() string { return KindProviderRefresh }

// ProviderRefreshWorker (#574) is the always-on provider-read umbrella. It keeps
// provider truth fresh without remote mutations: bounded event pulls with
// durable watermarks, the unknown-cohort reconcile (#632/#633/#665 — the ONE
// per-subscription verification path), and the CCBill DataLink bulk lane.
// Provider outages leave watermarks in place for the next scheduled/startup pass.
type ProviderRefreshWorker struct {
	river.WorkerDefaults[ProviderRefreshArgs]
	DB                  *db.DB
	Config              *config.Config
	Rails               config.RailMerchantAccountSet
	Clock               clockwork.Clock
	NMIClients          map[string]*nmi.NMIClient
	CCBillDataLink      *ccbill.DataLinkClient
	SolanaRPC           *solanaint.RPCClient
	EventLogService     *analytics.EventLogService
	DeferDelete         subscriptions.DeferredDeleteScheduler
	NotificationService *subscriptions.NotificationService

	Window          time.Duration
	SafetyLag       time.Duration
	InitialLookback time.Duration
	MaxWindows      int
}

func (ProviderRefreshWorker) Kind() string { return KindProviderRefresh }

func (w *ProviderRefreshWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *ProviderRefreshWorker) Work(ctx context.Context, job *river.Job[ProviderRefreshArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("provider refresh: db not configured")
	}
	if w.Config != nil && w.Config.IsProviderReadOnly() {
		log.WithContext(ctx).Warn("Readonly mode: provider refresh skipped (pure observer; no local convergence)")
		return nil
	}

	logger := log.WithContext(ctx).WithField("worker", KindProviderRefresh)
	stats := providerRefreshStats{}

	merchantIDs, err := w.DB.Gen(ctx).ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("provider refresh: list merchants: %w", err)
	}
	stats.Merchants = len(merchantIDs)

	fetchers, fetcherErr := reconcile.BuildFetchersWithOptions(w.Config, w.Rails, reconcile.FetcherClients{
		NMIClients:     w.NMIClients,
		CCBillDataLink: w.CCBillDataLink,
		SolanaRPC:      w.SolanaRPC,
	}, w.DB, reconcile.FetcherOptions{})
	if fetcherErr != nil {
		stats.LaneErrors++
		logger.WithError(fetcherErr).Warn("Provider Refresh: event lane unavailable")
	}
	// #665: per-subscription probe fallbacks for rows the bulk pull can't decide
	// (NULL period end / evidence outside the window). Snapshot sources only —
	// the unknown-reconcile core stays the one decider.
	probers := reconcile.BuildSubscriptionProbers(w.Rails, w.NMIClients)

	for _, mid := range merchantIDs {
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		if err := w.DB.RunInMerchantConn(mctx, func(tctx context.Context) error {
			if err := w.runCCBillDataLinkLane(tctx); err != nil {
				stats.CCBillErrors++
				logger.WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: CCBill DataLink lane failed")
			}
			if fetcherErr == nil {
				res := w.runEventRefresh(tctx, mid, fetchers)
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
			}
			// #633: reconcile the `unknown` cohort against provider truth (one
			// windowed bulk pull per rail + targeted per-sub probe fallbacks).
			// Tolerant of missing/failed fetchers — those rails' subs stay
			// `unknown` and are retried next pass (backoff).
			if err := w.runUnknownReconcile(tctx, mid, fetchers, probers); err != nil {
				stats.LaneErrors++
				logger.WithError(err).WithField("merchant_id", mid).Warn("Provider Refresh: unknown-cohort reconcile failed")
			}
			return nil
		}); err != nil {
			stats.LaneErrors++
			logger.WithError(err).WithField("merchant_id", mid).Error("Provider Refresh: merchant connection failed")
		}
	}

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
	return nil
}

func (w *ProviderRefreshWorker) runCCBillDataLinkLane(ctx context.Context) error {
	worker := CCBillReconcileWorker{
		DB:                  w.DB,
		DataLink:            w.CCBillDataLink,
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
	lc := subscriptions.NewSubscriptionLifecycleService(w.DB, nil, nil, nil, nil, nil, w.EventLogService, clock)
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

func refreshProviders(fetchers map[reconcile.Provider]reconcile.RailFetcher) []reconcile.Provider {
	allowed := map[reconcile.Provider]bool{
		reconcile.ProviderNMI:    true,
		reconcile.ProviderStripe: true,
		reconcile.ProviderCCBill: true,
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
   AND provider = $2::text
   AND event_domain = $3::text
   AND provider_account_key = COALESCE($4::uuid, '00000000-0000-0000-0000-000000000000'::uuid)
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
    merchant_id, provider, rail_merchant_account_id, event_domain, watermark_at,
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
    merchant_id, provider, rail_merchant_account_id, event_domain, watermark_at,
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
