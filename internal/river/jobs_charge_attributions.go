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
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
)

// chargeAttributionLimit bounds one run's reads; a larger import continues in
// the next run.
const chargeAttributionLimit = 20_000

// ChargeAttributionWorker reads the charge attributions one import declared
// from their provider, nmi.MaxQueryIDs per read, and records each charge on
// its declared subscription. The import queues it in its own transaction. Due
// rows are read; a deferred one (index lag, a failed read, a CCBill charge
// its pull has not read yet) snoozes the job until it is due, and the job ends
// when none is pending.
type ChargeAttributionWorker struct {
	WorkerDefaults[reconcile.ChargeAttributionArgs]
	DB          *db.DB
	Clock       clockwork.Clock
	NMIResolver railresolve.NMIClientResolver
	Rails       railresolve.Source
}

func (ChargeAttributionWorker) Kind() string { return reconcile.KindChargeAttributions }

func (w *ChargeAttributionWorker) Work(ctx context.Context, job *river.Job[reconcile.ChargeAttributionArgs]) error {
	if w.NMIResolver == nil {
		return errors.New("charge attributions: nmi resolver not wired")
	}
	mid := job.Args.MerchantID
	var wait time.Duration
	err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(mid), "charge attributions", func(ctx context.Context) error {
		now := w.Clock.Now().UTC()
		q := w.DB.Gen(ctx)
		pending, err := reconcile.PendingChargeAttributions(ctx, q, mid, job.Args.Declared)
		if err != nil {
			return err
		}
		due := pending
		for i, r := range pending {
			if r.NextAttemptAt.After(now) || i == chargeAttributionLimit {
				due = pending[:i]
				break
			}
		}
		if len(due) > 0 {
			if _, err := reconcile.ResolveChargeAttributions(ctx, w.DB, mid, due, func(ctx context.Context, psp uuid.UUID) (reconcile.AttributionReader, error) {
				client, ok, err := w.NMIResolver.ResolveNMIClient(ctx, mid, &psp)
				if err != nil || !ok || client == nil {
					return nil, fmt.Errorf("psp %s: nmi client unavailable: %v", psp, err)
				}
				return client, nil
			}, w.pulled, now); err != nil {
				return err
			}
			if pending, err = reconcile.PendingChargeAttributions(ctx, q, mid, job.Args.Declared); err != nil {
				return err
			}
		}
		if len(pending) > 0 {
			wait = max(pending[0].NextAttemptAt.Sub(now), time.Second)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("charge attributions: merchant %s: %w", mid, err)
	}
	if wait > 0 {
		return river.JobSnooze(wait)
	}
	return nil
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
