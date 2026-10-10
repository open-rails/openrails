package handlers

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	log "github.com/sirupsen/logrus"
)

// AdminChangeSubscription changes a subscription's price or seats at the
// customer's request, as the customer's own change would: an upgrade or more
// seats are charged now, merchant-initiated under the card's agreement; a
// downgrade or fewer seats wait for the renewal. The reason and the staff
// member are kept with the change and its charge, and the customer is told.
// CheckoutService runs as the subscription's customer so ownership and rail
// behavior match the customer's own change.
func AdminChangeSubscription(r *httprequest.Request) {
	req, customer, subscription, ok := adminChangeRequest(r)
	if !ok {
		return
	}
	if req.Reason == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "reason is required").WithParam("reason"))
		return
	}
	req.Invoker = resolveActorIdentity(r)
	req.IdempotencyKey = strings.TrimSpace(r.Header("Idempotency-Key"))
	// A replay is answered from the durable operation before the admission
	// guards: once the change committed, the subscription no longer passes them.
	resp, found, err := r.State.CheckoutService.ReplayTierChange(r.Request.Context(), req, customer)
	if !found && err == nil {
		if !adminTierChangeAdmissible(r, subscription) {
			return
		}
		resp, err = r.State.CheckoutService.ChangeSubscription(r.Request.Context(), req, customer)
	}
	if err != nil {
		logAdminTierChange(r, req, nil, err)
		writeChangeTierError(r, err)
		return
	}

	logAdminTierChange(r, req, resp, nil)
	writeTierChangeResponse(r, resp)
}

// AdminPreviewSubscriptionChange quotes a staff change: what it charges now
// and from the next renewal.
func AdminPreviewSubscriptionChange(r *httprequest.Request) {
	req, customer, subscription, ok := adminChangeRequest(r)
	if !ok || !adminTierChangeAdmissible(r, subscription) {
		return
	}

	resp, err := r.State.CheckoutService.PreviewSubscriptionChange(r.Request.Context(), req, customer)
	if err != nil {
		writeChangeTierError(r, err)
		return
	}

	r.SuccessJSON(resp)
}

func adminChangeRequest(
	r *httprequest.Request,
) (*checkout.SubscriptionChangeRequest, *checkout.UserIdentity, *models.Subscription, bool) {
	var body ChangeSubscriptionRequest
	if !r.BindJSON(&body) {
		return nil, nil, nil, false
	}
	req, ok := changeRequest(r, body.PriceID, body.Quantity)
	if !ok {
		return nil, nil, nil, false
	}
	if req.Reason = strings.TrimSpace(body.Reason); len(req.Reason) > 500 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "reason is at most 500 characters").WithParam("reason"))
		return nil, nil, nil, false
	}
	req.Staff = true
	if r.State.CheckoutService == nil || r.State.SubscriptionService == nil {
		r.ErrorCode(billing.CodeInternalError, "subscription service unavailable")
		return nil, nil, nil, false
	}
	subscription, err := r.State.SubscriptionService.GetByID(r.Request.Context(), req.SubscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			r.ErrorCode(billing.CodeResourceNotFound, "subscription not found")
			return nil, nil, nil, false
		}
		log.WithError(err).WithField("subscription_id", req.SubscriptionID).Error("admin subscription change: load subscription")
		r.ErrorCode(billing.CodeInternalError, "failed to retrieve subscription")
		return nil, nil, nil, false
	}
	if subscription.CustomerID == uuid.Nil {
		log.WithField("subscription_id", req.SubscriptionID).Error("admin subscription change: subscription has no customer")
		r.ErrorCode(billing.CodeInternalError, "subscription customer unavailable")
		return nil, nil, nil, false
	}
	user := &checkout.UserIdentity{ID: subscription.CustomerID.String()}
	if r.State.Contacts != nil {
		found, err := r.State.Contacts.Contacts(r.Request.Context(), billing.MerchantID(subscription.MerchantID), []uuid.UUID{subscription.CustomerID})
		if err != nil {
			log.WithError(err).WithField("subscription_id", req.SubscriptionID).Error("admin subscription change: customer contact")
			r.ErrorCode(billing.CodeServiceUnavailable, "the customer directory is unavailable")
			return nil, nil, nil, false
		}
		if email := found[subscription.CustomerID].Email; email != "" {
			user.Email = &email
		}
	}
	return req, user, subscription, true
}

// adminTierChangeAdmissible applies the operator-route guards to a new tier
// change; a replay never reaches them.
func adminTierChangeAdmissible(r *httprequest.Request, subscription *models.Subscription) bool {
	subscriptionID := subscription.ID
	if subscription.Status != models.StatusActive && subscription.Status != models.StatusPastDue {
		r.ErrorCode(billing.CodeResourceConflict, "only active or past-due subscriptions can change")
		return false
	}
	pending, err := subscriptions.PendingChange(r.Request.Context(), r.State.SubscriptionService.Database(), subscriptionID)
	if err != nil {
		log.WithError(err).WithField("subscription_id", subscriptionID).Error("admin subscription change: check the scheduled change")
		r.ErrorCode(billing.CodeInternalError, "failed to check the scheduled change")
		return false
	}
	// An engine subscription's service answers its own schedule: the same
	// downgrade replays, another is a typed refusal, an upgrade replaces it.
	if pending != nil && subscription.CollectionPolicy != models.CollectionPolicyEngine {
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, billing.CodeSubscriptionChangeAlreadyScheduled, "subscription already has a change scheduled"))
		return false
	}
	// CCBill changes on its own hosted page and Solana in the customer's
	// wallet: only the customer can take that step.
	if subscription.Rail == models.RailCCBill || subscription.Rail == models.RailSolana {
		r.APIError(api.Coded(billing.CodeCustomerActionRequired, "this subscription's tier changes only through the customer's own step on its rail"))
		return false
	}
	return true
}

func logAdminTierChange(
	r *httprequest.Request,
	req *checkout.SubscriptionChangeRequest,
	resp *checkout.TierChangeResponse,
	err error,
) {
	fields := log.Fields{
		"actor":           resolveActorIdentity(r),
		"event":           "admin_subscription_change",
		"subscription_id": req.SubscriptionID,
		"target_price_id": req.PriceID,
		"reason":          req.Reason,
	}
	if req.Quantity != nil {
		fields["target_quantity"] = *req.Quantity
	}
	if merchantID, merchantErr := merchant.Require(r.Request.Context()); merchantErr == nil {
		fields["merchant_id"] = merchantID
	}
	if resp != nil {
		fields["effective"] = resp.Effective
		fields["rail"] = resp.Rail
		fields["status"] = resp.Status
		if !resp.OperationID.IsZero() {
			fields["operation_id"] = resp.OperationID.String()
		}
	}
	if err != nil {
		log.WithError(err).WithFields(fields).Warn("admin subscription change failed")
		return
	}
	log.WithFields(fields).Info("admin subscription change processed")
}
