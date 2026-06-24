package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/vault"
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
		if repo.IsNotFound(err) {
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

	if !rails.IsNMIBackedRail(subscription.Rail) {
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
		case errors.Is(err, vault.ErrPaymentMethodNotFound):
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		case errors.Is(err, vault.ErrPaymentMethodAccessDenied):
			r.ErrorJSON(http.StatusForbidden, "You don't own this payment method")
			return
		default:
			log.WithError(err).WithFields(log.Fields{"payment_method_id": paymentMethodID, "user_id": targetUserID}).Error("Failed to validate payment method ownership")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to validate payment method")
			return
		}
	}

	if !rails.IsNMIBackedRail(paymentMethod.Rail) {
		r.ErrorJSON(http.StatusBadRequest, "Only NMI-backed payment methods can be used")
		return
	}
	if !rails.SameRail(paymentMethod.Rail, subscription.Rail) {
		r.ErrorJSON(http.StatusBadRequest, "Payment method belongs to a different payment provider")
		return
	}

	railName := string(subscription.Rail)
	nmiClient, ok := r.State.NMIClients[railName]
	if !ok {
		log.WithField("rail", railName).Error("NMI client not found for rail")
		r.ErrorJSON(http.StatusServiceUnavailable, "Payment rail not available")
		return
	}

	err = nmiClient.UpdateSubscriptionPaymentSource(subscription.RailSubscriptionID, paymentMethod.VaultID)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"subscription_id": subscription.ID, "rail_subscription": subscription.RailSubscriptionID, "new_vault_id": paymentMethod.VaultID, "payment_method_id": paymentMethod.ID}).Error("Failed to update subscription payment source with NMI")
		r.ErrorJSON(http.StatusBadGateway, "Failed to update payment method with payment rail")
		return
	}

	oldPaymentMethodID := subscription.PaymentMethodID
	subscription.PaymentMethodID = &paymentMethodID
	subscription.UpdatedAt = r.Clock.Now()

	if err := r.State.SubscriptionService.Update(ctx, subscription); err != nil {
		log.WithError(err).WithFields(log.Fields{"subscription_id": subscription.ID, "payment_method_id": paymentMethodID}).Error("NMI updated but local DB update failed - manual reconciliation needed")
		r.ErrorJSON(http.StatusInternalServerError, "Payment method updated but failed to save locally")
		return
	}

	log.WithFields(log.Fields{"subscription_id": subscription.ID, "rail_subscription": subscription.RailSubscriptionID, "old_payment_method_id": oldPaymentMethodID, "new_payment_method_id": paymentMethodID, "user_id": targetUserID}).Info("Subscription payment method updated successfully")

	r.SuccessJSON(map[string]any{"success": true, "message": "Payment method updated successfully", "subscription_id": subscription.ID.String(), "payment_method_id": paymentMethodID.String()})
}
