package handlers

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// AdminChangeTier changes a subscription's tier on the customer's behalf. The
// merchant permission gate authorizes the operator; CheckoutService still runs
// as the subscription customer so ownership and rail behavior stay identical
// to self-service tier changes.
func AdminChangeTier(r *httprequest.Request) {
	req, customer, subscription, ok := adminTierChangeRequest(r)
	if !ok {
		return
	}
	req.IdempotencyKey = strings.TrimSpace(r.Header("Idempotency-Key"))
	// A replay is answered from the durable operation before the admission
	// guards: once the change committed, the subscription no longer passes them.
	resp, found, err := r.State.CheckoutService.ReplayTierChange(r.Request.Context(), req, customer)
	if !found && err == nil {
		if !adminTierChangeAdmissible(r, subscription) {
			return
		}
		resp, err = r.State.CheckoutService.TierChange(r.Request.Context(), req, customer)
	}
	if err != nil {
		logAdminTierChange(r, req, nil, err)
		writeChangeTierError(r, err)
		return
	}

	logAdminTierChange(r, req, resp, nil)
	writeTierChangeResponse(r, resp)
}

// AdminChangeTierPreview returns the same non-mutating proration preview as the
// customer self-service route.
func AdminChangeTierPreview(r *httprequest.Request) {
	req, customer, subscription, ok := adminTierChangeRequest(r)
	if !ok || !adminTierChangeAdmissible(r, subscription) {
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
) (*checkout.TierChangeRequest, *checkout.UserIdentity, *models.Subscription, bool) {
	var body ChangeTierRequest
	if !r.BindJSON(&body) {
		return nil, nil, nil, false
	}
	if id, err := openrails.ParsePriceID(body.PriceID); err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid price_id")
		return nil, nil, nil, false
	}

	typedSubscriptionID, err := openrails.ParseSubscriptionID(r.Param("id"))
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription ID")
		return nil, nil, nil, false
	}
	subscriptionID := typedSubscriptionID.UUID()
	if r.State.CheckoutService == nil || r.State.SubscriptionService == nil || r.State.RepriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "subscription service unavailable")
		return nil, nil, nil, false
	}

	subscription, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
			return nil, nil, nil, false
		}
		log.WithError(err).WithField("subscription_id", subscriptionID).Error("admin tier change: load subscription")
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve subscription")
		return nil, nil, nil, false
	}
	if subscription.CustomerID == uuid.Nil {
		log.WithField("subscription_id", subscriptionID).Error("admin tier change: subscription has no customer")
		r.ErrorJSON(http.StatusInternalServerError, "subscription customer unavailable")
		return nil, nil, nil, false
	}
	return &checkout.TierChangeRequest{
			PriceID:        body.PriceID,
			SubscriptionID: subscriptionID,
		}, &checkout.UserIdentity{
			ID:    subscription.CustomerID.String(),
			Email: subscription.UserEmail,
		}, subscription, true
}

// adminTierChangeAdmissible applies the operator-route guards to a new tier
// change; a replay never reaches them.
func adminTierChangeAdmissible(r *httprequest.Request, subscription *models.Subscription) bool {
	subscriptionID := subscription.ID
	if subscription.Status != models.StatusActive && subscription.Status != models.StatusPastDue {
		r.ErrorJSON(http.StatusConflict, "only active or past-due subscriptions can change tier")
		return false
	}
	if subscription.ScheduledPriceID != nil {
		r.ErrorJSON(http.StatusConflict, "subscription already has a tier change scheduled")
		return false
	}
	if subscription.Rail == models.RailCCBill {
		r.ErrorJSON(http.StatusBadRequest, "CCBill tier changes require customer self-service")
		return false
	}
	if subscription.Rail == models.RailSolana {
		r.ErrorJSON(http.StatusBadRequest, "Solana tier changes require the customer's wallet signature")
		return false
	}

	scheduled := models.RepriceStatusScheduled
	reprices, err := r.State.RepriceService.List(r.Request.Context(), subscriptions.SubscriptionRepriceFilter{
		SubscriptionID: &subscriptionID,
		Status:         &scheduled,
	}, 1, 0)
	if err != nil {
		log.WithError(err).WithField("subscription_id", subscriptionID).Error("admin tier change: check scheduled reprices")
		r.ErrorJSON(http.StatusInternalServerError, "failed to check scheduled price changes")
		return false
	}
	if len(reprices) > 0 {
		r.ErrorJSON(http.StatusConflict, "subscription already has a scheduled price change")
		return false
	}
	return true
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
		if resp.OperationID != "" {
			fields["operation_id"] = resp.OperationID
		}
	}
	if err != nil {
		log.WithError(err).WithFields(fields).Warn("admin subscription tier change failed")
		return
	}
	log.WithFields(fields).Info("admin subscription tier change processed")
}
