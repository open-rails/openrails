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
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/writeposture"
)

const KindMerchantHealth = "openrails.merchant_health"

// MerchantHealthInterval is the health pass's cadence: its findings are
// rates over hours and days, and deadlines an hour late changes nothing.
const MerchantHealthInterval = time.Hour

// Merchants with activity in these windows, or a deadline passed in them,
// are looked at: a rate spike needs the last day's attempts, and a lapse or
// an operation that changed since the pass before last, with room for an
// outage. Engine renewals are held past an allowance of at least five
// minutes; operations are stuck past two hours.
const (
	healthActiveWindow = 25 * time.Hour
	heldAllowanceMin   = 5 * time.Minute
	stuckAgeMin        = 2 * time.Hour
	healthMerchantPage = 500
)

type MerchantHealthArgs struct{}

func (MerchantHealthArgs) Kind() string { return KindMerchantHealth }

func (MerchantHealthArgs) InsertOpts() river.InsertOpts { return periodPass(MerchantHealthInterval) }

// MerchantHealthWorker evaluates the health findings (recovery funnel, held
// renewals, payment health, the unverified backlog, stuck and abandoned
// provider operations) and timed access lapses, only for merchants with
// activity, an open health finding or a passed deadline.
type MerchantHealthWorker struct {
	WorkerDefaults[MerchantHealthArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	// Alerts sends requires_review findings to the operator. nil = none.
	Alerts *alerting.Service
}

func (MerchantHealthWorker) Kind() string { return KindMerchantHealth }

func (w MerchantHealthWorker) Work(ctx context.Context, _ *river.Job[MerchantHealthArgs]) error {
	engine := converge.NewConvergeEngine(w.DB, w.Clock)
	now := engine.Now()
	if w.Alerts != nil {
		engine.Notifier = w.Alerts
	}
	postures := writeposture.View{Config: w.Config, DB: w.DB}
	var workErr error
	after := uuid.Nil
	for {
		page, err := w.DB.GenDirectory().ListHealthMerchants(ctx, gen.ListHealthMerchantsParams{
			AfterMerchantID: after, ActiveSince: now.Add(-healthActiveWindow), FindingTypes: converge.HealthFindingTypes(),
			HeldBefore: now.Add(-heldAllowanceMin), StuckBefore: now.Add(-stuckAgeMin), LapsedSince: now.Add(-healthActiveWindow), Now: now,
			MerchantLimit: healthMerchantPage,
		})
		if err != nil {
			return errors.Join(workErr, fmt.Errorf("merchant health: list merchants: %w", err))
		}
		for _, mid := range page {
			id := billing.MerchantID(mid)
			if err := w.DB.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
				if postures.Posture(ctx, mid).ReadOnly() {
					return nil
				}
				_, err := engine.Health(ctx, id)
				return err
			}); err != nil {
				workErr = errors.Join(workErr, fmt.Errorf("merchant health: merchant %s: %w", mid, err))
			}
		}
		if len(page) < healthMerchantPage {
			return workErr
		}
		after = page[len(page)-1]
	}
}
