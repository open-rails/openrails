package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
)

const (
	KindChargeAttributions = "openrails.charge_attributions"

	// ChargeAttributionInterval is how often due attributions are read.
	ChargeAttributionInterval = 15 * time.Minute
	// chargeAttributionLimit bounds one merchant's share of a pass.
	chargeAttributionLimit     = 20_000
	chargeAttributionMerchants = 200
)

// ChargeAttributionArgs runs one pass over due charge attributions.
type ChargeAttributionArgs struct{}

func (ChargeAttributionArgs) Kind() string { return KindChargeAttributions }

// ChargeAttributionWorker reads each due declared charge attribution from its
// provider once, up to nmi.MaxQueryIDs per read, and records the charge on its
// declared subscription. Only due rows are read; a failed read backs off. A
// CCBill attribution is left to the provider pull while DataLink reads it.
type ChargeAttributionWorker struct {
	river.WorkerDefaults[ChargeAttributionArgs]
	DB          *db.DB
	Clock       clockwork.Clock
	NMIResolver railresolve.NMIClientResolver
	Rails       railresolve.Source
}

func (ChargeAttributionWorker) Kind() string { return KindChargeAttributions }

func (w *ChargeAttributionWorker) Work(ctx context.Context, _ *river.Job[ChargeAttributionArgs]) error {
	if w.NMIResolver == nil {
		return errors.New("charge attributions: nmi resolver not wired")
	}
	now := w.Clock.Now().UTC()
	merchantIDs, err := w.DB.GenDirectory().ListDueChargeAttributionMerchants(ctx, gen.ListDueChargeAttributionMerchantsParams{Now: now, MerchantLimit: chargeAttributionMerchants})
	if err != nil {
		return fmt.Errorf("charge attributions: list merchants: %w", err)
	}
	var workErr error
	for _, mid := range merchantIDs {
		err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(mid), "charge attributions", func(ctx context.Context) error {
			_, err := reconcile.ResolveChargeAttributions(ctx, w.DB, mid, func(ctx context.Context, psp uuid.UUID) (reconcile.AttributionReader, error) {
				client, ok, err := w.NMIResolver.ResolveNMIClient(ctx, mid, &psp)
				if err != nil || !ok || client == nil {
					return nil, fmt.Errorf("psp %s: nmi client unavailable: %v", psp, err)
				}
				return client, nil
			}, w.pulled, now, chargeAttributionLimit)
			return err
		})
		if err != nil {
			workErr = errors.Join(workErr, fmt.Errorf("merchant %s: %w", mid, err))
		}
	}
	return workErr
}

// pulled reports whether the provider refresh reads the PSP's charges: CCBill
// through DataLink, when its credentials are declared.
func (w *ChargeAttributionWorker) pulled(ctx context.Context, psp uuid.UUID, rail string) (bool, error) {
	if rail != string(models.RailCCBill) || w.Rails == nil {
		return false, nil
	}
	resolved, err := w.Rails.RailConfig(db.WithPSPID(ctx, psp), rail, "")
	if errors.Is(err, railresolve.ErrRailNotArmed) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return resolved.CCBill != nil && resolved.CCBill.DataLinkUsername != "", nil
}
