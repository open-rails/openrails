package webhooks

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// revokeStripeMembership ends a Stripe membership's access now and, in the same
// transaction, queues the durable remote cancel when Stripe owns its billing.
// feedback sees the locked row. after runs in the transaction once the local
// cancel is applied.
func revokeStripeMembership(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, subID uuid.UUID, cancelType models.CancelType,
	feedback func(*models.Subscription) string, after func(context.Context, *db.DB, *models.Subscription) error) error {
	var notes []*models.NotificationQueue
	err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := database.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subID)
		if err != nil {
			return fmt.Errorf("lock stripe subscription %s: %w", subID, err)
		}
		reason := feedback(sub)
		rail := models.RailStripe
		res, err := lc.CancelMembershipTx(ctx, d, &subscriptions.CancelMembershipParams{
			SubscriptionID: &subID, Rail: &rail, CancelType: cancelType, CancelFeedback: &reason, RevokeAccess: true,
		})
		if err != nil {
			return err
		}
		notes = res.Notifications
		if err := intents.NewProviderCancelScheduler(d, nil, intents.OriginAdmin, reason).ScheduleProviderCancel(ctx, sub, timeutil.FirstClock(lc.Clock()).Now()); err != nil {
			return fmt.Errorf("queue stripe cancel for %s: %w", subID, err)
		}
		if after != nil {
			return after(ctx, d, sub)
		}
		return nil
	})
	if err != nil {
		return err
	}
	lc.DispatchNotifications(ctx, notes)
	return nil
}
