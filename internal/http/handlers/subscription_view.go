package handlers

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// subscriptionView projects a merchant-side subscription read onto the shared
// Client DTO.
func subscriptionView(in *subscriptions.AdminSubscriptionResponse, now time.Time) billing.Subscription {
	return subscriptions.SubscriptionView(in.Subscription, in.Price, in.Dunning, now)
}

// withMandates sets each subscription's MandateID: the agreement it renews
// under. It answers a 500 on failure.
func withMandates(r *httprequest.Request, subs []billing.Subscription) bool {
	if len(subs) == 0 {
		return true
	}
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if err != nil {
		r.InternalError("merchant unresolved", err)
		return false
	}
	ids := make([]uuid.UUID, len(subs))
	for i, s := range subs {
		ids[i] = s.ID.UUID()
	}
	byID, err := mandates.ForSubscriptions(ctx, r.State.DB.Gen(ctx), mid.UUID(), ids)
	if err != nil {
		r.InternalError("failed to read subscription mandates", err)
		return false
	}
	for i := range subs {
		if m, ok := byID[subs[i].ID.UUID()]; ok {
			id := billing.MandateID(m)
			subs[i].MandateID = &id
		}
	}
	return true
}

// withMRR sets each subscription's MRR, which the merchant routes serve. It
// answers a 500 on failure.
func withMRR(r *httprequest.Request, subs []billing.Subscription) bool {
	if len(subs) == 0 {
		return true
	}
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if err != nil {
		r.InternalError("merchant unresolved", err)
		return false
	}
	ids := make([]uuid.UUID, len(subs))
	for i, s := range subs {
		ids[i] = s.ID.UUID()
	}
	byID, err := subscriptions.MRRViews(ctx, r.State.DB.Gen(ctx), mid.UUID(), ids)
	if err != nil {
		r.InternalError("failed to read subscription MRR", err)
		return false
	}
	for i := range subs {
		subs[i].MRR = byID[subs[i].ID.UUID()]
	}
	return true
}

// writeMerchantSubscriptions answers a merchant route's page of
// subscriptions with their MRR and mandates.
func writeMerchantSubscriptions(r *httprequest.Request, page billing.ListPage[billing.Subscription]) {
	if withMRR(r, page.Items) {
		writeSubscriptions(r, page)
	}
}

// writeMerchantSubscriptionOne answers a merchant route's subscription with
// its MRR and mandate.
func writeMerchantSubscriptionOne(r *httprequest.Request, sub billing.Subscription) {
	one := []billing.Subscription{sub}
	if withMRR(r, one) {
		writeSubscription(r, one[0])
	}
}

// writeSubscriptions answers a page of subscriptions with their mandates.
func writeSubscriptions(r *httprequest.Request, page billing.ListPage[billing.Subscription]) {
	if withMandates(r, page.Items) {
		r.SuccessJSON(page)
	}
}

// writeSubscription answers one subscription with its mandate.
func writeSubscription(r *httprequest.Request, sub billing.Subscription) {
	one := []billing.Subscription{sub}
	if withMandates(r, one) {
		r.SuccessJSON(one[0])
	}
}
