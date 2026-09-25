package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/billing/lifecycle"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// ReplaceMethod decides a new card on a locked membership: one awaiting a
// card resumes dunning, and a delinquent one retries at the next due pass
// instead of waiting out the old card's schedule.
func ReplaceMethod(sub *models.Subscription, now time.Time) error {
	if _, err := Transition(sub, lifecycle.MethodReplaced{}, now); err != nil {
		return fmt.Errorf("replace method on %s: %w", sub.ID, err)
	}
	if sub.Status == models.StatusPastDue {
		sub.NextRetryAt = &now
	}
	return nil
}

// WakeForReplacedMethod applies ReplaceMethod to every delinquent engine
// membership charged to paymentMethodID whose card was replaced in place.
func WakeForReplacedMethod(ctx context.Context, d *db.DB, merchantID, paymentMethodID uuid.UUID, now time.Time) error {
	ids, err := d.Gen(ctx).ListEngineSubscriptionsToWake(ctx, gen.ListEngineSubscriptionsToWakeParams{MerchantID: merchantID, PaymentMethodID: paymentMethodID, Now: now})
	if err != nil {
		return fmt.Errorf("list memberships to wake: %w", err)
	}
	repo := NewSubscriptionRepo(d)
	for _, id := range ids {
		sub, err := repo.GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}
		waiting := sub.Status == models.StatusAwaitingMethod || (sub.Status == models.StatusPastDue && (sub.NextRetryAt == nil || sub.NextRetryAt.After(now)))
		if sub.CollectionPolicy != models.CollectionPolicyEngine || sub.PaymentMethodID == nil || *sub.PaymentMethodID != paymentMethodID || !waiting {
			continue
		}
		if err := ReplaceMethod(sub, now); err != nil {
			return err
		}
		if err := repo.UpdateAt(ctx, sub, now); err != nil {
			return err
		}
	}
	return nil
}
