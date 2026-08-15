package handlers

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// AdminChangeTier changes a subscription's tier on the customer's behalf. The
// merchant permission gate authorizes the operator; CheckoutService still runs
// as the subscription customer so ownership and rail behavior stay identical
// to self-service tier changes.
func AdminChangeTier(r *httprequest.Request) {
	req, customer, ok := adminTierChangeRequest(r)
	if !ok {
		return
	}

	req.IdempotencyKey = middleware.IdempotencyKeyFromRequest(r.Request)
	resp, err := r.State.CheckoutService.TierChange(r.Request.Context(), req, customer)
	if err != nil {
		logAdminTierChange(r, req, nil, err)
		writeChangeTierError(r, err)
		return
	}

	logAdminTierChange(r, req, resp, nil)
	r.SuccessJSON(resp)
}

// AdminChangeTierPreview returns the same non-mutating proration preview as the
// customer self-service route.
func AdminChangeTierPreview(r *httprequest.Request) {
	req, customer, ok := adminTierChangeRequest(r)
	if !ok {
		return
	}

	resp, err := r.State.CheckoutService.TierChangePreview(r.Request.Context(), req, customer)
	if err != nil {
		writeChangeTierError(r, err)
		return
	}

	r.SuccessJSON(resp)
}

func adminTierChangeRequest(
	r *httprequest.Request,
) (*checkout.TierChangeRequest, *checkout.UserIdentity, bool) {
	var body ChangeTierRequest
	if !r.BindJSON(&body) {
		return nil, nil, false
	}

	subscriptionID, err := api.ParseSubscriptionID(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription ID")
		return nil, nil, false
	}
	if r.State.CheckoutService == nil || r.State.SubscriptionService == nil || r.State.RepriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "subscription service unavailable")
		return nil, nil, false
	}

	subscription, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
			return nil, nil, false
		}
		log.WithError(err).WithField("subscription_id", subscriptionID).Error("admin tier change: load subscription")
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve subscription")
		return nil, nil, false
	}
	if subscription.CustomerID == uuid.Nil {
		log.WithField("subscription_id", subscriptionID).Error("admin tier change: subscription has no customer")
		r.ErrorJSON(http.StatusInternalServerError, "subscription customer unavailable")
		return nil, nil, false
	}
	if subscription.Status != models.StatusActive && subscription.Status != models.StatusPastDue {
		r.ErrorJSON(http.StatusConflict, "only active or past-due subscriptions can change tier")
		return nil, nil, false
	}
	if subscription.ScheduledPriceID != nil {
		r.ErrorJSON(http.StatusConflict, "subscription already has a tier change scheduled")
		return nil, nil, false
	}
	if subscription.Rail == models.RailCCBill {
		r.ErrorJSON(http.StatusBadRequest, "CCBill tier changes require customer self-service")
		return nil, nil, false
	}
	if subscription.Rail == models.RailSolana {
		r.ErrorJSON(http.StatusBadRequest, "Solana tier changes require the customer's wallet signature")
		return nil, nil, false
	}

	scheduled := models.RepriceStatusScheduled
	reprices, err := r.State.RepriceService.List(r.Request.Context(), subscriptions.SubscriptionRepriceFilter{
		SubscriptionID: &subscriptionID,
		Status:         &scheduled,
	}, 1, 0)
	if err != nil {
		log.WithError(err).WithField("subscription_id", subscriptionID).Error("admin tier change: check scheduled reprices")
		r.ErrorJSON(http.StatusInternalServerError, "failed to check scheduled price changes")
		return nil, nil, false
	}
	if len(reprices) > 0 {
		r.ErrorJSON(http.StatusConflict, "subscription already has a scheduled price change")
		return nil, nil, false
	}

	return &checkout.TierChangeRequest{
			PriceID:        body.PriceID,
			SubscriptionID: subscriptionID,
		}, &checkout.UserIdentity{
			ID:    subscription.CustomerID.String(),
			Email: subscription.UserEmail,
		}, true
}

func logAdminTierChange(
	r *httprequest.Request,
	req *checkout.TierChangeRequest,
	resp *checkout.TierChangeResponse,
	err error,
) {
	fields := log.Fields{
		"actor":           resolveActorIdentity(r),
		"event":           "admin_subscription_tier_change",
		"subscription_id": req.SubscriptionID,
		"target_price_id": req.PriceID,
	}
	if merchantID, merchantErr := merchant.Require(r.Request.Context()); merchantErr == nil {
		fields["merchant_id"] = merchantID
	}
	if resp != nil {
		fields["action"] = resp.Action
		fields["rail"] = resp.Payment.Rail
		fields["status"] = resp.Status
	}
	if err != nil {
		log.WithError(err).WithFields(fields).Warn("admin subscription tier change failed")
		return
	}
	log.WithFields(fields).Info("admin subscription tier change processed")
}
