package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

type updateSubscriptionPaymentMethodBody struct {
	PaymentMethodID string `json:"payment_method_id" binding:"required"`
}

func UpdateSubscriptionPaymentMethod(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorJSON(http.StatusUnauthorized, "Authentication required")
		return
	}
	updateSubscriptionPaymentMethod(r, user.ID, true)
}

func AdminUpdateSubscriptionPaymentMethod(r *httprequest.Request) {
	updateSubscriptionPaymentMethod(r, "", false)
}

func updateSubscriptionPaymentMethod(r *httprequest.Request, authenticatedUserID string, enforceOwnership bool) {
	subscriptionIDStr := r.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorJSON(http.StatusBadRequest, "subscription ID required")
		return
	}

	subscriptionID, err := api.ParseSubscriptionID(subscriptionIDStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}

	var req updateSubscriptionPaymentMethodBody
	if !r.BindJSON(&req) {
		return
	}

	paymentMethodID, err := api.ParsePaymentMethodID(req.PaymentMethodID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid payment_method_id format")
		return
	}

	ctx, cancel := context.WithTimeout(r.Request.Context(), 15*time.Second)
	defer cancel()

	subscription, err := r.State.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			r.ErrorJSON(http.StatusNotFound, "Subscription not found")
			return
		}
		log.WithError(err).WithField("subscription_id", subscriptionID).Error("Failed to get subscription")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to retrieve subscription")
		return
	}

	targetUserID := subscription.CustomerID.String()
	if enforceOwnership && targetUserID != authenticatedUserID {
		r.ErrorJSON(http.StatusForbidden, "You don't own this subscription")
		return
	}

	if !rails.IsNMI(subscription.Rail) {
		r.ErrorJSON(http.StatusBadRequest, "Only NMI-backed subscriptions can have their payment method updated")
		return
	}

	if subscription.Status != models.StatusActive && subscription.Status != models.StatusPastDue {
		r.ErrorJSON(http.StatusBadRequest, "Cannot update payment method for cancelled subscriptions")
		return
	}

	paymentMethod, err := r.State.PaymentMethodService.ValidatePaymentMethodOperation(ctx, paymentMethodID, targetUserID)
	if err != nil {
		switch {
		case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound):
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		case errors.Is(err, paymentmethods.ErrPaymentMethodAccessDenied):
			r.ErrorJSON(http.StatusForbidden, "You don't own this payment method")
			return
		default:
			log.WithError(err).WithFields(log.Fields{"payment_method_id": paymentMethodID, "user_id": targetUserID}).Error("Failed to validate payment method ownership")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to validate payment method")
			return
		}
	}

	if !rails.IsNMI(paymentMethod.Rail) {
		r.ErrorJSON(http.StatusBadRequest, "Only NMI-backed payment methods can be used")
		return
	}
	if !rails.SameRail(paymentMethod.Rail, subscription.Rail) {
		r.ErrorJSON(http.StatusBadRequest, "Payment method belongs to a different payment provider")
		return
	}
	if !subscriptions.PaymentMethodMatchesSubscriptionProvider(paymentMethod, subscription) {
		r.ErrorJSON(http.StatusBadRequest, "Payment method belongs to a different payment provider account")
		return
	}

	// Pre-flight: resolve the rail read-only so misconfiguration surfaces as
	// 503 immediately (the intent handler re-resolves at execution time).
	_, providerKey, ok, err := subscriptions.NMIClientForExistingSubscription(ctx, r.State.DB, r.State.NMIClients, subscription)
	if err != nil {
		log.WithError(err).WithField("subscription_id", subscription.ID).Error("failed to resolve NMI provider account for subscription")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to resolve payment rail")
		return
	}
	if !ok {
		log.WithFields(log.Fields{"rail": subscription.Rail, "provider_account": providerKey}).Error("NMI client not found for subscription provider account")
		r.ErrorJSON(http.StatusServiceUnavailable, "Payment rail not available")
		return
	}

	// #674: the swap goes through the durable nmi_payment_source_update intent
	// (write-through) — a lost provider response can never leave local and NMI
	// silently billing different cards; the intent ledger converges them.
	origin, originReason := intents.OriginUser, "user payment-method swap"
	if !enforceOwnership {
		origin, originReason = intents.OriginAdmin, "admin payment-method swap"
	}
	oldPaymentMethodID := subscription.PaymentMethodID
	out, err := r.State.PaymentSourceUpdateIntents.ExecutePaymentSourceUpdate(ctx, subscription, paymentMethod, origin, originReason)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"subscription_id": subscription.ID, "payment_method_id": paymentMethodID}).Error("Failed to post payment-source update intent")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to update payment method")
		return
	}
	switch {
	case out.Done:
		log.WithFields(log.Fields{"subscription_id": subscription.ID, "rail_subscription": subscription.RailSubscriptionID, "old_payment_method_id": oldPaymentMethodID, "new_payment_method_id": paymentMethodID, "user_id": targetUserID}).Info("Subscription payment method updated successfully")
		r.SuccessJSON(map[string]any{"success": true, "message": "Payment method updated successfully", "subscription_id": subscription.ID.String(), "payment_method_id": paymentMethodID.String()})
	case out.Terminal:
		log.WithFields(log.Fields{"subscription_id": subscription.ID, "rail_subscription": subscription.RailSubscriptionID, "new_vault_id": paymentMethod.RailCustomerRef, "payment_method_id": paymentMethod.ID, "reason": out.Reason}).Error("Failed to update subscription payment source with NMI")
		r.ErrorJSON(http.StatusBadGateway, "Failed to update payment method with payment rail")
	default:
		// Ambiguous/parked: neither success nor decline — the durable intent
		// finishes out-of-band and a retried request maps onto the SAME intent.
		log.WithFields(log.Fields{"subscription_id": subscription.ID, "payment_method_id": paymentMethodID, "reason": out.Reason}).Warn("Payment-source update unresolved inline; intent ledger will converge")
		r.ErrorJSON(http.StatusConflict, intents.ErrPaymentSourceUpdateProcessing.Error())
	}
}
