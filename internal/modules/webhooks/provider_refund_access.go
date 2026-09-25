package webhooks

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// providerRefundAccess is the one rule every rail applies to a refund made at
// the provider (its dashboard or API) rather than through OpenRails: the
// merchant's provider_refund_access policy decides whether the refunded
// charge's access ends. A provider-billed NMI membership also ends at NMI (its
// schedule delete rides the durable, destructive-gated operation; while the
// switch is off it is held and an operator finding is raised).
type providerRefundAccess struct {
	DB        *db.DB
	Clock     clockwork.Clock
	Lifecycle *subscriptions.SubscriptionLifecycleService
}

func (p providerRefundAccess) apply(ctx context.Context, rail models.Rail, original *models.Payment, refundedTotal int64, reason string) (bool, error) {
	if p.DB == nil || original == nil {
		return false, nil
	}
	revoke, err := merchantconfig.ProviderRefundRevokes(ctx, p.DB, refundedTotal, original.Amount)
	if err != nil || !revoke {
		return false, err
	}
	now := timeutil.FirstClock(p.Clock).Now().UTC()
	if original.SubscriptionID == nil {
		if err := entitlements.NewEntitlementService(p.DB, p.Clock).EndActiveByPayment(ctx, original.ID, models.EntitlementRevokeRefund); err != nil {
			return true, fmt.Errorf("revoke one-off entitlements after provider refund: %w", err)
		}
		if _, err := productaccess.NewService(p.DB, p.Clock).RevokeProductAccessByPayment(ctx, original.ID, models.ProductAccessRevokeRefund); err != nil {
			return true, fmt.Errorf("revoke product access after provider refund: %w", err)
		}
		return true, nil
	}
	sub, err := subscriptions.NewSubscriptionRepo(p.DB).GetByID(ctx, *original.SubscriptionID)
	if err != nil {
		return true, fmt.Errorf("load refunded subscription: %w", err)
	}
	live := sub.Status.Live()
	if live && sub.CollectionPolicy != models.CollectionPolicyEngine && subscriptions.NeedsProviderScheduleDelete(sub) {
		// The refund already happened at NMI; the membership ends regardless of
		// the switch, whose state only holds the schedule delete.
		if _, err := subscriptions.RequireProviderCancelArmed(ctx, p.DB, sub, true); err != nil {
			return true, err
		}
		return true, p.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			d := p.DB.NewWithPgxTx(tx)
			repo := subscriptions.NewSubscriptionRepo(d)
			locked, err := repo.GetByIDForUpdate(ctx, sub.ID)
			if err != nil {
				return err
			}
			if locked.Status.Live() {
				cancelType := models.CancelTypeMerchant
				locked.Status, locked.CancelledAt, locked.EndedAt, locked.CancelType, locked.CancelFeedback = models.StatusCancelled, &now, &now, &cancelType, &reason
				locked.DeletionScheduledAt = &now
				locked.ClearRetrySchedule()
				locked.MarkLifecycleDecision("provider_refund_revoke")
				if err := repo.UpdateAt(ctx, locked, now); err != nil {
					return err
				}
				if err := intents.NewProviderCancelScheduler(d, nil, intents.OriginAdmin, reason).ScheduleNMIDelete(ctx, locked.CustomerID.String(), locked.ID, now); err != nil {
					return err
				}
			}
			return p.revokeSubscription(ctx, d, locked, original, now)
		})
	}
	if live && p.Lifecycle != nil && intents.StripeOwned(sub) {
		// Stripe keeps billing until told otherwise: the remote cancel is
		// queued with the revoke.
		return true, revokeStripeMembership(ctx, p.DB, p.Lifecycle, sub.ID, models.CancelTypeMerchant,
			func(*models.Subscription) string { return reason },
			func(ctx context.Context, d *db.DB, locked *models.Subscription) error {
				return p.revokeSubscription(ctx, d, locked, original, now)
			})
	}
	if live && p.Lifecycle != nil {
		if err := p.Lifecycle.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: &sub.ID, Rail: &rail, CancelType: models.CancelTypeMerchant, CancelFeedback: &reason, RevokeAccess: true,
		}); err != nil {
			return true, fmt.Errorf("cancel subscription after provider refund: %w", err)
		}
	}
	return true, p.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return p.revokeSubscription(ctx, p.DB.NewWithPgxTx(tx), sub, original, now)
	})
}

// revokeSubscription ends the refunded membership's standing and paid access;
// revoking terminates the grants too, so convergence never re-projects them.
func (p providerRefundAccess) revokeSubscription(ctx context.Context, d *db.DB, sub *models.Subscription, original *models.Payment, now time.Time) error {
	ent := entitlements.NewEntitlementService(d, p.Clock)
	if err := ent.RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, now, models.EntitlementRevokeRefund, models.EntitlementSourceSubscription, models.EntitlementSourceGrace); err != nil {
		return err
	}
	return ent.EndActiveByPayment(ctx, original.ID, models.EntitlementRevokeRefund)
}
