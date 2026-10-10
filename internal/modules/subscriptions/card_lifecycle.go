package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// ApplyCardLifecycle applies a card event and carries it to the subscriptions
// the card pays, in d's transaction. The notices it returns are dispatched
// after commit.
func (s *SubscriptionLifecycleService) ApplyCardLifecycle(ctx context.Context, d *db.DB, ev paymentmethods.CardEvent) (paymentmethods.CardLifecycle, []*models.NotificationQueue, error) {
	life, err := paymentmethods.ApplyCardLifecycle(ctx, d.Gen(ctx), ev)
	if err != nil {
		return life, nil, err
	}
	notices, err := s.FollowCardChange(ctx, d, life, ev.At)
	return life, notices, err
}

// FollowCardChange carries a card's change to the subscriptions it pays. A
// reissue wakes those waiting on the card. A brand change, closure or contact
// advice asks their members to act; renewals then wait for the customer
// (mandates hold or end), and a closed card never cancels a subscription.
func (s *SubscriptionLifecycleService) FollowCardChange(ctx context.Context, d *db.DB, life paymentmethods.CardLifecycle, now time.Time) ([]*models.NotificationQueue, error) {
	switch {
	case life.Change.NeedsCustomer():
		return s.AskForCard(ctx, d, life.Method.MerchantID, life.Method.ID, now)
	case life.Reissued:
		return nil, WakeForReplacedMethod(ctx, d, life.Method.MerchantID, life.Method.ID, now)
	}
	return nil, nil
}

// AskForCard queues the update-your-card notice for each live subscription
// the card pays, once per paid period.
func (s *SubscriptionLifecycleService) AskForCard(ctx context.Context, d *db.DB, merchantID, methodID uuid.UUID, now time.Time) ([]*models.NotificationQueue, error) {
	ids, err := d.Gen(ctx).ListLiveSubscriptionsOnMethod(ctx, gen.ListLiveSubscriptionsOnMethodParams{MerchantID: merchantID, PaymentMethodID: methodID})
	if err != nil {
		return nil, err
	}
	repo := NewSubscriptionRepo(d)
	var out []*models.NotificationQueue
	for _, id := range ids {
		sub, err := repo.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("ask for a card on %s: %w", id, err)
		}
		queued, err := s.ApplyEffects(ctx, d, sub, []lifecycle.Effect{lifecycle.Notify{Kind: lifecycle.NoticeUpdateMethod}}, now, EffectOptions{})
		if err != nil {
			return nil, err
		}
		out = append(out, queued...)
	}
	return out, nil
}
