package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
)

// AdoptProviderPrice moves a provider-run subscription onto the provider
// price its provider now bills, with that price's access choice, and settles
// a scheduled change the move fulfils. A move to another product ends the old
// product's grace and opens the new product's access in the same
// transaction. sub is locked in d's transaction.
func AdoptProviderPrice(ctx context.Context, d *db.DB, sub *models.Subscription, price *models.Price, now time.Time, clock clockwork.Clock) error {
	if err := requireTx(d); err != nil {
		return err
	}
	oldProduct := sub.ProductID
	sub.PriceID, sub.ProductID = price.ID, price.ProductID
	// The new price's immutable access choice is the one accepted with it.
	sub.AccessDurationHoursSnapshot = price.AccessDurationHours
	sub.ProviderAccessDefaultSnapshot = price.ProviderAccessDefault
	if err := ProviderMovedPrice(ctx, d, sub.ID, price.ID, now); err != nil {
		return fmt.Errorf("settle the scheduled change: %w", err)
	}
	if err := NewSubscriptionRepo(d).UpdateAt(ctx, sub, now); err != nil {
		return fmt.Errorf("write the provider price: %w", err)
	}
	if oldProduct == price.ProductID {
		return nil
	}
	sourceType, sourceID := models.AccessSourceGrace, sub.ID.String()
	if err := entitlements.NewEntitlementService(d, clock).RevokeAccess(ctx, entitlements.RevokeAccessParams{
		UserID: sub.CustomerID.String(), ProductID: oldProduct, SourceType: &sourceType, SourceID: &sourceID, Reason: models.AccessRevokeDowngrade,
	}); err != nil {
		return fmt.Errorf("revoke the moved product's grace: %w", err)
	}
	ledger := grants.New(d.Gen(ctx), sub.MerchantID)
	ledger.SetClock(func() time.Time { return now.UTC() })
	if err := ledger.DeriveSubscriptionGrant(ctx, gen.ListUngrantedSubscriptionsRow{
		ID: sub.ID, CustomerID: sub.CustomerID, ProductID: sub.ProductID, Status: string(sub.Status),
		CurrentPeriodStartsAt: sub.CurrentPeriodStartsAt, CurrentPeriodEndsAt: sub.CurrentPeriodEndsAt, StartedAt: sub.StartedAt, EndedAt: sub.EndedAt,
		AccessDurationHoursSnapshot: models.IntPtrTo32(sub.AccessDurationHoursSnapshot), ProviderAccessDefaultSnapshot: sub.ProviderAccessDefaultSnapshot,
	}); err != nil {
		return fmt.Errorf("open the moved product's access: %w", err)
	}
	return nil
}
