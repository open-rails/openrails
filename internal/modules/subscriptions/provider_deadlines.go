package subscriptions

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
)

// A provider-billed subscription whose deadline passed with no word from its
// provider is asked by id: it moves to unverified, keeping access. A clock
// reading is never evidence, so neither ends or charges anything.

// AskProviderPastGrace moves a provider-billed past_due row whose grace ended
// at or before now with no retry scheduled. Reports whether it moved.
func (s *SubscriptionLifecycleService) AskProviderPastGrace(ctx context.Context, d *db.DB, sub *models.Subscription, now time.Time) (bool, error) {
	period := sub.CurrentPeriodEndsAt
	return s.Decide(ctx, d, sub.ID, lifecycle.DunningStale{}, func(_ context.Context, _ *db.DB, locked *models.Subscription) (bool, error) {
		return locked.Status == models.StatusPastDue && locked.CollectionPolicy != models.CollectionPolicyEngine && locked.NextRetryAt == nil &&
			locked.GraceEndsAt != nil && !locked.GraceEndsAt.After(now) && samePeriodEnd(period, locked.CurrentPeriodEndsAt), nil
	})
}

// AskProviderOverdueRenewal moves an active provider-billed row still on the
// period that ended at periodEnd with no payment since. Reports whether it
// moved.
func (s *SubscriptionLifecycleService) AskProviderOverdueRenewal(ctx context.Context, d *db.DB, id uuid.UUID, periodEnd time.Time) (bool, error) {
	return s.Decide(ctx, d, id, lifecycle.RenewalOverdue{}, func(ctx context.Context, d *db.DB, locked *models.Subscription) (bool, error) {
		if locked.Status != models.StatusActive || locked.CollectionPolicy == models.CollectionPolicyEngine || !samePeriodEnd(&periodEnd, locked.CurrentPeriodEndsAt) {
			return false, nil
		}
		paid, err := d.Gen(ctx).SubscriptionHasCompletedPayment(ctx, gen.SubscriptionHasCompletedPaymentParams{MerchantID: locked.MerchantID, SubscriptionID: locked.ID, Since: locked.CurrentPeriodEndsAt})
		return !paid, err
	})
}
