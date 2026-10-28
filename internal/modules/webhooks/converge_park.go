package webhooks

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/modules/parking"
	"github.com/open-rails/openrails/internal/reconcile"
)

// heldProductIndexes are the one-live-subscription indexes a customer who
// already holds the product (or its tier group) trips.
var heldProductIndexes = map[string]bool{"subscriptions_customer_id_product_id_key": true, "subscriptions_customer_id_tier_group_key": true}

// productHeld reports whether a landing failed because the customer already
// holds the product.
func productHeld(err error) bool {
	constraint, ok := db.UniqueViolation(err)
	return ok && heldProductIndexes[constraint]
}

// parkSubscription parks a live provider subscription converge cannot land
// and notifies its finding. The job completes: the park waits for its cause.
func parkSubscription(ctx context.Context, database *db.DB, findings reconcile.FindingNotifier, s parking.Subject, now time.Time) error {
	res, err := parking.Park(ctx, database, s, now)
	if err != nil {
		return err
	}
	fields := log.Fields{"rail": s.Rail, "subscription_reference": s.SubscriptionRef, "reason": s.Reason, "price_ref": s.PriceRef, "customer_ref": s.CustomerRef}
	if res.Deferred {
		log.WithContext(ctx).WithFields(fields).Info("converge: subscription not landable; the PSP's import owns it until it is pulled and not staged")
		return nil
	}
	log.WithContext(ctx).WithFields(fields).Warn("converge: parked a live provider subscription it cannot land")
	if res.Finding != nil && findings != nil {
		if err := findings.NotifyFinding(ctx, reconcile.FindingRecordFromRow(*res.Finding)); err != nil && ctx.Err() == nil {
			log.WithContext(ctx).WithError(err).WithFields(fields).Warn("converge: notifying the park's finding failed; it stays in the console")
		}
	}
	return nil
}

// closePark ends the subscription's open park, if any.
func closePark(ctx context.Context, database *db.DB, railSubID string, by billing.ParkRelease, now time.Time) error {
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return err
	}
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return parking.Close(ctx, gen.New(tx), pspID, railSubID, by, now)
	})
}

// observePrice records the provider price an unmapped subscription bills
// when it was never observed, so the park's price is listed with its terms.
// stated is the copy a subscription carries (NMI), recorded when the
// provider no longer has the price.
func observePrice(ctx context.Context, objects *providerobjects.Observer, ref string, stated *providerobjects.Object) error {
	if objects == nil || objects.Merchants == nil || ref == "" {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return err
	}
	scope, found, err := objects.Merchants.PSPScopeByID(ctx, mid, pspID)
	if err != nil || !found {
		return err
	}
	if obj, err := objects.Get(ctx, scope, providerobjects.Price, ref); err != nil || obj != nil {
		return err
	}
	_, err = objects.ObserveByID(ctx, scope, providerobjects.Price, ref, stated, providerobjects.ViaRead)
	return err
}
