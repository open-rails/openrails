package subscriptions

import (
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// View projects a customer's own subscription onto the shared DTO, with the
// scheduled plan change and the access it grants. Derived flags are evaluated
// at the response's read time.
func (r *UserSubscriptionResponse) View() billing.Subscription {
	out := SubscriptionView(r.Subscription, r.Price, r.Dunning, r.EvaluationTime())
	if out.ScheduledChange != nil {
		out.ScheduledChange.Price = r.ScheduledPrice.PublicView()
		out.ScheduledChange.Product = r.ScheduledProduct.Summary()
	}
	out.Access = r.Access
	return out
}

// SubscriptionView is the one projection of a subscription row onto the
// shared billing.Subscription: the merchant and self routes both serve it.
// dunning is the case DunningViews read, nil when none.
func SubscriptionView(sub *models.Subscription, price *models.Price, dunning *billing.SubscriptionDunning, now time.Time) billing.Subscription {
	out := billing.Subscription{
		CollectionPolicy: string(sub.CollectionPolicy), Dunning: dunning, DeletionScheduledAt: sub.DeletionScheduledAt,
		ID: billing.SubscriptionID(sub.ID), CustomerID: billing.CustomerID(sub.CustomerID), ProductID: billing.ProductID(sub.ProductID), PriceID: billing.PriceID(sub.PriceID),
		Quantity: sub.Quantity,
		PSPID:    billing.PSPID(sub.PspID), Rail: string(sub.Rail), RailSubscriptionID: normalize.OptionalString(sub.RailSubscriptionID), Status: billing.SubscriptionStatus(sub.Status),
		StartedAt: sub.StartedAt, EndedAt: sub.EndedAt, CurrentPeriodStartsAt: sub.CurrentPeriodStartsAt, CurrentPeriodEndsAt: sub.CurrentPeriodEndsAt,
		CanceledAt: sub.CanceledAt, CancelFeedback: sub.CancelFeedback, CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
		PaymentMethodID: (*billing.PaymentMethodID)(sub.PaymentMethodID),
		Resumable:       Resumable(sub, now), CancelScheduled: CancelScheduled(sub, now), CancelMode: string(CancelModeFor(sub, now)),
		CancelPortalURL: CancelPortalURL(sub, now),
		Price:           price.PublicView(), Product: sub.Product.Summary(), Card: subscriptionCardView(sub.PaymentMethod),
	}
	out.ScheduledChange = sub.ScheduledChange.View()
	if sub.CancelType != nil {
		v := string(*sub.CancelType)
		out.CancelType = &v
	}
	return out
}

// subscriptionCardView is the card on the subscription's payment method, from
// the linked payment_methods row alone: never a provider fetch.
func subscriptionCardView(pm *models.PaymentMethod) *billing.CardDetails {
	if pm == nil {
		return nil
	}
	return pm.Card.Details()
}
