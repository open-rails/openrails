package subscriptions

import (
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
)

// View projects a customer's own subscription onto the shared DTO, with the
// scheduled plan change and the access it grants. Derived flags are evaluated
// at the response's read time.
func (r *UserSubscriptionResponse) View() billing.Subscription {
	out := SubscriptionView(r.Subscription, r.Price, r.EvaluationTime())
	out.ScheduledPrice = subscriptionPriceView(r.ScheduledPrice)
	out.ScheduledProduct = r.ScheduledProduct.Summary()
	out.Access = r.Access
	return out
}

// SubscriptionView is the one projection of a subscription row onto the
// shared billing.Subscription: the merchant and self routes both serve it.
func SubscriptionView(sub *models.Subscription, price *models.Price, now time.Time) billing.Subscription {
	out := billing.Subscription{
		CollectionPolicy: string(sub.CollectionPolicy),
		LastRetryAt:      sub.LastRetryAt, RetryAttempts: sub.RetryAttempts, NextRetryAt: sub.NextRetryAt, GraceEndsAt: sub.GraceEndsAt, DeletionScheduledAt: sub.DeletionScheduledAt,
		ID: billing.SubscriptionID(sub.ID), CustomerID: (billing.CustomerID(sub.CustomerID)).String(), ProductID: billing.ProductID(sub.ProductID).String(), PriceID: billing.PriceID(sub.PriceID).String(),
		PSPID: sub.PspID.String(), Rail: string(sub.Rail), RailSubscriptionID: sub.RailSubscriptionID, Status: string(sub.Status),
		StartedAt: sub.StartedAt, EndedAt: sub.EndedAt, CurrentPeriodStartsAt: sub.CurrentPeriodStartsAt, CurrentPeriodEndsAt: sub.CurrentPeriodEndsAt,
		CancelledAt: sub.CancelledAt, CancelFeedback: sub.CancelFeedback, CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
		PaymentMethodID: (*billing.PaymentMethodID)(sub.PaymentMethodID),
		Resumable:       Resumable(sub, now), CancelScheduled: CancelScheduled(sub, now), CancelMode: string(CancelModeFor(sub, now)),
		CancelPortalURL: CancelPortalURL(sub, now),
		Price:           subscriptionPriceView(price), Product: sub.Product.Summary(), Card: subscriptionCardView(sub.PaymentMethod),
	}
	if sub.ScheduledPriceID != nil {
		id := billing.PriceID(*sub.ScheduledPriceID).String()
		out.ScheduledPriceID = &id
	}
	if sub.CancelType != nil {
		v := string(*sub.CancelType)
		out.CancelType = &v
	}
	return out
}

func subscriptionPriceView(p *models.Price) *billing.SubscriptionPrice {
	if p == nil {
		return nil
	}
	return &billing.SubscriptionPrice{ID: billing.PriceID(p.ID).String(), Key: p.Key, ProductID: billing.ProductID(p.ProductID).String(), UnitAmount: p.Amount, Currency: p.Currency, AutoRenew: p.AutoRenew, AccessDurationHours: p.AccessDurationHours, Archived: p.Archived}
}

// subscriptionCardView is the card on the subscription's payment method, from
// the linked payment_methods row alone: never a provider fetch.
func subscriptionCardView(pm *models.PaymentMethod) *billing.CardDetails {
	if pm == nil {
		return nil
	}
	return pm.Card.Details()
}
