package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/pkg/merchant"
)

// KindCatalogReconciliationPull is the River kind for the alert-only catalog
// reconciliation loop (issue #209). It reads the active Stripe and NMI accounts
// completely and records standing catalog findings. It NEVER mutates Stripe,
// NMI, or the catalog rows.
//
// CCBill is intentionally NOT reconciled here: CCBill has no catalog-list API,
// so enumeration is structurally impossible (see the package doc on
// internal/service/catalog_drift.go). CCBill links stay manual-only.
//
// It runs the same catalog.RunDriftPass as Service.RunCatalogReconciliation.
const KindCatalogReconciliationPull = "openrails.catalog_reconciliation_pull"

type CatalogReconciliationPullArgs struct{}

func (CatalogReconciliationPullArgs) Kind() string { return KindCatalogReconciliationPull }

// CatalogReconciliationPullWorker runs one pull-and-diff pass across both Stripe
// and NMI. Each provider pass is independently skipped if that rail is
// unconfigured.
type CatalogReconciliationPullWorker struct {
	river.WorkerDefaults[CatalogReconciliationPullArgs]
	DB     *db.DB
	Config *config.Config
	// Rails resolves the ctx merchant's armed rail credentials (#788).
	Rails railresolve.Source
	// NMIResolver arms the ctx merchant's NMI client (#788).
	NMIResolver money.NMIClientResolver
	// StripeBaseURL overrides the Stripe API root; empty in production.
	StripeBaseURL string
}

func (CatalogReconciliationPullWorker) Kind() string { return KindCatalogReconciliationPull }

// catalogReconcileMerchantBatch bounds each page of the pass; the work queue is the
// armed-PSP set, so a pass scales with merchants on a reconcilable rail.
const catalogReconcileMerchantBatch = 1000

// Work fans the pull-and-diff pass out over the merchants armed on a
// reconcilable rail, one merchant scope each (or#877, found by FC-16).
//
// It used to run ONE pass on the bare job context: ProductService/PriceService
// GetAll read s.db.Gen(ctx) with no merchant, so under openrails_app the local
// catalog came back EMPTY and every provider product/price was diffed against
// nothing. Alert-only or not, "no drift" was never an answer this worker had
// actually computed — and the NMI leg silently skipped itself entirely, because
// merchant.Require(ctx) could not succeed on a bare context.
func (w CatalogReconciliationPullWorker) Work(ctx context.Context, job *river.Job[CatalogReconciliationPullArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("catalog reconciliation: db not configured")
	}
	if w.Config == nil {
		return fmt.Errorf("catalog reconciliation: config not configured")
	}
	_ = job

	var after *uuid.UUID
	var sweepErr error
	for {
		merchantIDs, err := w.DB.GenDirectory().ListRailArmedMerchants(ctx, gen.ListRailArmedMerchantsParams{
			Rails:         []string{string(models.RailStripe), string(models.RailNMI)},
			MerchantLimit: catalogReconcileMerchantBatch, AfterMerchantID: after,
		})
		if err != nil {
			return preferSweepError(sweepErr, fmt.Errorf("catalog reconciliation: list armed merchants: %w", err))
		}
		for _, mid := range merchantIDs {
			if mid == nil {
				continue
			}
			after = mid
			merchantID := merchant.ID(*mid)
			progress.Mark(ctx, "catalog reconciliation merchant "+merchantID.String())
			if err := w.DB.RunInMerchantScope(ctx, merchantID, "catalog reconciliation", func(mctx context.Context) error { return w.reconcileMerchant(mctx) }); err != nil {
				sweepErr = preferSweepError(sweepErr, fmt.Errorf("catalog reconciliation merchant %s: %w", merchantID, err))
				log.WithContext(ctx).WithError(err).WithField("merchant_id", merchantID.String()).Error("CatalogReconciliation: merchant pass failed; continuing")
			}
		}
		if len(merchantIDs) < catalogReconcileMerchantBatch {
			break
		}
	}
	return sweepErr
}

// reconcileMerchant runs one merchant's shared pull-and-diff pass inside its
// scope. Only the active Stripe and NMI accounts are read; findings of other
// accounts are left untouched.
func (w CatalogReconciliationPullWorker) reconcileMerchant(ctx context.Context) error {
	var sources catalog.DriftSources
	stripe, ok, err := catalog.ActiveDriftPSP(ctx, w.Rails, models.RailStripe)
	if err != nil {
		return err
	}
	if ok {
		sources.StripePSPID = stripe.ID
		sources.Stripe = catalog.PinnedStripeLister{PSPID: stripe.ID, Service: &catalog.StripeCatalogService{Config: w.Config, Rails: w.Rails, BaseURL: w.StripeBaseURL}}
	}
	account, ok, err := catalog.ActiveDriftPSP(ctx, w.Rails, models.RailNMI)
	if err != nil {
		return err
	}
	if ok && w.NMIResolver != nil {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		client, armed, err := w.NMIResolver.ResolveNMIClient(ctx, mid.UUID(), &account.ID)
		if err != nil {
			return fmt.Errorf("catalog reconciliation: resolve nmi client: %w", err)
		}
		if armed {
			sources.NMIPSPID, sources.NMI = account.ID, client
		}
	}
	if sources.Stripe == nil && sources.NMI == nil {
		log.WithContext(ctx).Info("CatalogReconciliation: no readable stripe or nmi account; skipping")
		return nil
	}
	now := time.Now().UTC()
	report, err := catalog.RunDriftPass(ctx, w.DB, sources, now)
	if err != nil {
		return fmt.Errorf("catalog reconciliation: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"scanned_products":  report.ScannedProducts,
		"scanned_prices":    report.ScannedPrices,
		"scanned_nmi_plans": report.ScannedNMIPlans,
		"new_events":        report.NewEvents,
		"resolved_events":   report.ResolvedEvents,
	}).Info("CatalogReconciliation: completed pull-and-diff pass")
	return nil
}
