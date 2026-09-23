package subscriptions

import (
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
)

// View projects a customer's own subscription onto the shared DTO, with the
// scheduled plan change and the access it grants. Derived flags are evaluated
// at the response's read time.
func (r *UserSubscriptionResponse) View() openrails.Subscription {
	out := SubscriptionView(r.Subscription, r.Price, r.EvaluationTime())
	out.ScheduledPrice = subscriptionPriceView(r.ScheduledPrice)
	out.ScheduledProduct = ProductView(r.ScheduledProduct)
	out.Access = r.Access
	return out
}

// SubscriptionView is the one projection of a subscription row onto the
// shared openrails.Subscription: the merchant and self routes both serve it.
func SubscriptionView(sub *models.Subscription, price *models.Price, now time.Time) openrails.Subscription {
	out := openrails.Subscription{
		CollectionPolicy: string(sub.CollectionPolicy),
		LastRetryAt:      sub.LastRetryAt, RetryAttempts: sub.RetryAttempts, NextRetryAt: sub.NextRetryAt, GraceEndsAt: sub.GraceEndsAt, DeletionScheduledAt: sub.DeletionScheduledAt,
		ID: openrails.SubscriptionID(sub.ID), CustomerID: (openrails.CustomerID(sub.CustomerID)).String(), ProductID: openrails.ProductID(sub.ProductID).String(), PriceID: openrails.PriceID(sub.PriceID).String(),
		PSPID: sub.PspID.String(), Rail: string(sub.Rail), RailSubscriptionID: sub.RailSubscriptionID, Status: string(sub.Status),
		StartedAt: sub.StartedAt, EndedAt: sub.EndedAt, CurrentPeriodStartsAt: sub.CurrentPeriodStartsAt, CurrentPeriodEndsAt: sub.CurrentPeriodEndsAt,
		CancelledAt: sub.CancelledAt, CancelFeedback: sub.CancelFeedback, CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
		PaymentMethodID: (*openrails.PaymentMethodID)(sub.PaymentMethodID),
		Resumable:       Resumable(sub, now), CancelScheduled: CancelScheduled(sub, now), CancelMode: string(CancelModeFor(sub, now)),
		CancelPortalURL: CancelPortalURL(sub, now),
		Price:           subscriptionPriceView(price), Product: ProductView(sub.Product), Card: subscriptionCardView(sub.PaymentMethod),
	}
	if sub.ScheduledPriceID != nil {
		id := openrails.PriceID(*sub.ScheduledPriceID).String()
		out.ScheduledPriceID = &id
	}
	if sub.CancelType != nil {
		v := string(*sub.CancelType)
		out.CancelType = &v
	}
	return out
}

func subscriptionPriceView(p *models.Price) *openrails.SubscriptionPrice {
	if p == nil {
		return nil
	}
	return &openrails.SubscriptionPrice{ID: openrails.PriceID(p.ID).String(), Key: p.Key, ProductID: openrails.ProductID(p.ProductID).String(), UnitAmount: p.Amount, Currency: p.Currency, AutoRenew: p.AutoRenew, AccessDurationHours: p.AccessDurationHours, Archived: p.Archived}
}

// ProductView is the customer-facing product shape shared by subscriptions and payments.
func ProductView(p *models.Product) *openrails.SubscriptionProduct {
	if p == nil {
		return nil
	}
	return &openrails.SubscriptionProduct{ID: openrails.ProductID(p.ID).String(), Key: p.Key, DisplayName: p.DisplayName, Description: p.Description, TierGroup: p.TierGroup, TierRank: p.TierRank, Archived: p.Archived}
}

// subscriptionCardView is the card on the subscription's payment method, from
// the linked payment_methods row alone: never a provider fetch.
func subscriptionCardView(pm *models.PaymentMethod) *openrails.SubscriptionCard {
	if pm == nil {
		return nil
	}
	card := &openrails.SubscriptionCard{}
	if pm.CardType != nil {
		card.Brand = *pm.CardType
	}
	if pm.LastFour != nil {
		card.Last4 = *pm.LastFour
	}
	if card.Brand == "" && card.Last4 == "" {
		return nil
	}
	if pm.ExpiryDate != nil {
		if month, year, ok := sharedformat.ParseExpiry(*pm.ExpiryDate); ok {
			card.ExpMonth = &month
			card.ExpYear = &year
		}
	}
	return card
}
