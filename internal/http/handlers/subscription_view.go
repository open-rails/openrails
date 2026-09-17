package handlers

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
)

// subscriptionView projects a subscription onto the shared Client DTO. Ids of
// resource kinds pkg/api prefixes (subscription, product, price, payment
// method, payment) travel in their prefixed form, the same form PaymentMethod
// and CheckoutSession already use; customer and PSP ids are plain UUIDs.
func subscriptionView(in *subscriptions.AdminSubscriptionResponse, now time.Time) openrails.Subscription {
	sub := in.Subscription
	out := openrails.Subscription{
		LastRetryAt: sub.LastRetryAt, RetryAttempts: sub.RetryAttempts, NextRetryAt: sub.NextRetryAt, GraceEndsAt: sub.GraceEndsAt, DeletionScheduledAt: sub.DeletionScheduledAt,
		ID: api.FormatSubscriptionID(sub.ID), CustomerID: sub.CustomerID.String(), ProductID: api.FormatProductID(sub.ProductID), PriceID: api.FormatPriceID(sub.PriceID),
		PSPID: sub.PspID.String(), Rail: string(sub.Rail), RailSubscriptionID: sub.RailSubscriptionID, Status: string(sub.Status),
		StartedAt: sub.StartedAt, EndedAt: sub.EndedAt, CurrentPeriodStartsAt: sub.CurrentPeriodStartsAt, CurrentPeriodEndsAt: sub.CurrentPeriodEndsAt,
		CancelledAt: sub.CancelledAt, CancelFeedback: sub.CancelFeedback, CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
		ScheduledPriceID: formatOptionalID(sub.ScheduledPriceID, api.FormatPriceID), PaymentMethodID: formatOptionalID(sub.PaymentMethodID, api.FormatPaymentMethodID),
		Resumable: subscriptions.Resumable(sub, now), CancelScheduled: subscriptions.CancelScheduled(sub, now), CancelMode: string(subscriptions.CancelModeFor(sub, now)),
	}
	if sub.CancelType != nil {
		v := string(*sub.CancelType)
		out.CancelType = &v
	}
	if in.Price != nil {
		p := in.Price
		out.Price = &openrails.SubscriptionPrice{ID: api.FormatPriceID(p.ID), Key: p.Key, ProductID: api.FormatProductID(p.ProductID), Amount: p.Amount, Currency: p.Currency, AutoRenew: p.AutoRenew, AccessDurationHours: p.AccessDurationHours, Archived: p.Archived}
	}
	if sub.Product != nil {
		p := sub.Product
		out.Product = &openrails.SubscriptionProduct{ID: api.FormatProductID(p.ID), Key: p.Key, DisplayName: p.DisplayName, Description: p.Description, TierGroup: p.TierGroup, TierRank: p.TierRank, Archived: p.Archived}
	}
	for _, p := range in.Payments {
		out.Payments = append(out.Payments, openrails.SubscriptionPayment{ID: api.FormatPaymentID(p.ID), Status: p.Status, Amount: p.Amount, Currency: p.Currency, Rail: string(p.Rail), TransactionID: p.TransactionID, PurchasedAt: p.PurchasedAt})
	}
	return out
}

func formatOptionalID(id *uuid.UUID, format func(uuid.UUID) string) *string {
	if id == nil {
		return nil
	}
	v := format(*id)
	return &v
}
