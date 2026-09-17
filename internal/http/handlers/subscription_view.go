package handlers

import (
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// subscriptionView projects a subscription onto the shared Client DTO.
func subscriptionView(in *subscriptions.AdminSubscriptionResponse, now time.Time) openrails.Subscription {
	sub := in.Subscription
	out := openrails.Subscription{
		LastRetryAt: sub.LastRetryAt, RetryAttempts: sub.RetryAttempts, NextRetryAt: sub.NextRetryAt, GraceEndsAt: sub.GraceEndsAt, DeletionScheduledAt: sub.DeletionScheduledAt,
		ID: openrails.SubscriptionID(sub.ID), CustomerID: openrails.CustomerID(sub.CustomerID), ProductID: openrails.ProductID(sub.ProductID), PriceID: openrails.PriceID(sub.PriceID),
		PSPID: sub.PspID.String(), Rail: string(sub.Rail), RailSubscriptionID: sub.RailSubscriptionID, Status: string(sub.Status),
		StartedAt: sub.StartedAt, EndedAt: sub.EndedAt, CurrentPeriodStartsAt: sub.CurrentPeriodStartsAt, CurrentPeriodEndsAt: sub.CurrentPeriodEndsAt,
		CancelledAt: sub.CancelledAt, CancelFeedback: sub.CancelFeedback, CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
		ScheduledPriceID: (*openrails.PriceID)(sub.ScheduledPriceID), PaymentMethodID: (*openrails.PaymentMethodID)(sub.PaymentMethodID),
		Resumable: subscriptions.Resumable(sub, now), CancelScheduled: subscriptions.CancelScheduled(sub, now), CancelMode: string(subscriptions.CancelModeFor(sub, now)),
	}
	if sub.CancelType != nil {
		v := string(*sub.CancelType)
		out.CancelType = &v
	}
	if in.Price != nil {
		p := in.Price
		out.Price = &openrails.SubscriptionPrice{ID: openrails.PriceID(p.ID), Key: p.Key, ProductID: openrails.ProductID(p.ProductID), Amount: p.Amount, Currency: p.Currency, AutoRenew: p.AutoRenew, AccessDurationHours: p.AccessDurationHours, Archived: p.Archived}
	}
	if sub.Product != nil {
		p := sub.Product
		out.Product = &openrails.SubscriptionProduct{ID: openrails.ProductID(p.ID), Key: p.Key, DisplayName: p.DisplayName, Description: p.Description, TierGroup: p.TierGroup, TierRank: p.TierRank, Archived: p.Archived}
	}
	for _, p := range in.Payments {
		out.Payments = append(out.Payments, openrails.SubscriptionPayment{ID: openrails.PaymentID(p.ID), Status: p.Status, Amount: p.Amount, Currency: p.Currency, Rail: string(p.Rail), TransactionID: p.TransactionID, PurchasedAt: p.PurchasedAt})
	}
	return out
}
