package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/processors"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

// UpdateSubscriptionPaymentMethodBody is the request body for PUT /v1/me/subscriptions/:id/payment-method
type UpdateSubscriptionPaymentMethodBody struct {
	PaymentMethodID string `json:"payment_method_id" binding:"required"`
}

// UpdateSubscriptionPaymentMethod changes which stored payment method a subscription uses.
// PUT /v1/me/subscriptions/:id/payment-method
// Request: { payment_method_id: 'uuid' }
//
// Validations:
//   - User must own the subscription (from JWT)
//   - User must own the target payment method
//   - Payment method must be NMI-backed
//   - Subscription must be NMI-backed (not CCBill/Solana)
//   - Subscription must be active or past_due (not cancelled)
func UpdateSubscriptionPaymentMethod(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorJSON(http.StatusUnauthorized, "Authentication required")
		return
	}

	// Parse subscription ID from path
	subscriptionIDStr := r.GinCtx.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorJSON(http.StatusBadRequest, "subscription ID required")
		return
	}

	subscriptionID, err := api.ParseSubscriptionID(subscriptionIDStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}

	// Bind and validate request body
	var req UpdateSubscriptionPaymentMethodBody
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

	// 1. Get and validate subscription ownership
	subscription, err := r.State.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "Subscription not found")
			return
		}
		log.WithError(err).WithField("subscription_id", subscriptionID).Error("Failed to get subscription")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to retrieve subscription")
		return
	}

	if subscription.UserID != user.ID {
		r.ErrorJSON(http.StatusForbidden, "You don't own this subscription")
		return
	}

	// 2. Validate subscription is NMI-backed
	if !processors.IsNMIBackedProcessor(subscription.Processor) {
		r.ErrorJSON(http.StatusBadRequest, "Only NMI-backed subscriptions can have their payment method updated")
		return
	}

	// 3. Validate subscription status (must be active or past_due)
	if subscription.Status != models.StatusActive && subscription.Status != models.StatusPastDue {
		r.ErrorJSON(http.StatusBadRequest, "Cannot update payment method for cancelled subscriptions")
		return
	}

	// 4. Get and validate payment method ownership
	paymentMethod, err := r.State.PaymentMethodService.ValidatePaymentMethodOperation(ctx, paymentMethodID, user.ID)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrPaymentMethodNotFound):
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		case errors.Is(err, services.ErrPaymentMethodAccessDenied):
			r.ErrorJSON(http.StatusForbidden, "You don't own this payment method")
			return
		default:
			log.WithError(err).WithFields(log.Fields{
				"payment_method_id": paymentMethodID,
				"user_id":           user.ID,
			}).Error("Failed to validate payment method ownership")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to validate payment method")
			return
		}
	}

	// 5. Validate payment method is NMI-backed
	if !processors.IsNMIBackedProcessor(paymentMethod.Processor) {
		r.ErrorJSON(http.StatusBadRequest, "Only NMI-backed payment methods can be used")
		return
	}

	// 7. Get the NMI client for this processor
	processorName := string(subscription.Processor)
	nmiClient, ok := r.State.NMIClients[processorName]
	if !ok {
		log.WithField("processor", processorName).Error("NMI client not found for processor")
		r.ErrorJSON(http.StatusServiceUnavailable, "Payment processor not available")
		return
	}

	// 8. Call NMI API to update subscription's customer vault ID
	err = nmiClient.UpdateSubscriptionPaymentSource(subscription.ProcessorSubscriptionID, paymentMethod.VaultID)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"subscription_id":        subscription.ID,
			"processor_subscription": subscription.ProcessorSubscriptionID,
			"new_vault_id":           paymentMethod.VaultID,
			"payment_method_id":      paymentMethod.ID,
		}).Error("Failed to update subscription payment source with NMI")
		r.ErrorJSON(http.StatusBadGateway, "Failed to update payment method with payment processor")
		return
	}

	// 9. Update local subscription record with new payment method ID
	subscription.PaymentMethodID = &paymentMethodID
	subscription.UpdatedAt = r.Clock.Now()

	if err := r.State.SubscriptionService.Update(ctx, subscription); err != nil {
		// NMI was updated but local DB failed - log this for manual reconciliation
		log.WithError(err).WithFields(log.Fields{
			"subscription_id":   subscription.ID,
			"payment_method_id": paymentMethodID,
		}).Error("NMI updated but local DB update failed - manual reconciliation needed")
		r.ErrorJSON(http.StatusInternalServerError, "Payment method updated but failed to save locally")
		return
	}

	// 10. Log success for audit purposes
	log.WithFields(log.Fields{
		"subscription_id":        subscription.ID,
		"processor_subscription": subscription.ProcessorSubscriptionID,
		"old_payment_method_id":  subscription.PaymentMethodID,
		"new_payment_method_id":  paymentMethodID,
		"user_id":                user.ID,
	}).Info("Subscription payment method updated successfully")

	// 11. Return success response with updated subscription info
	r.SuccessJSON(map[string]any{
		"success":           true,
		"message":           "Payment method updated successfully",
		"subscription_id":   subscription.ID.String(),
		"payment_method_id": paymentMethodID.String(),
	})
}
