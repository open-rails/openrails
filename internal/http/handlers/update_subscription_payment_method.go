package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails"

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

type updateSubscriptionPaymentMethodBody = openrails.UpdateSubscriptionPaymentMethodRequest

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

	typedSubscriptionID, err := openrails.ParseSubscriptionID(subscriptionIDStr)
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()

	var req updateSubscriptionPaymentMethodBody
	if !r.BindJSON(&req) {
		return
	}

	if req.PaymentMethodID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "Invalid payment_method_id format")
		return
	}
	paymentMethodID := req.PaymentMethodID.UUID()

	ctx, cancel := r.Budget(15 * time.Second)
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
		r.ErrorJSON(http.StatusNotFound, "Subscription not found")
		return
	}

	if subscription.CollectionPolicy == models.CollectionPolicyEngine {
		if err := r.State.SubscriptionLifecycleService.UpdateEnginePaymentMethod(ctx, subscription.ID, subscription.CustomerID, paymentMethodID); err != nil {
			writeRefusal(r, err, "Failed to select payment method")
			return
		}
		r.SuccessJSON(map[string]any{"success": true, "message": "Payment method updated successfully", "subscription_id": openrails.SubscriptionID(subscription.ID), "payment_method_id": openrails.PaymentMethodID(paymentMethodID)})
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
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
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
		writePaymentMethodPSPMismatch(r)
		return
	}
	if err := subscriptions.ValidatePaymentMethodSourceCustody(paymentMethod); err != nil {
		writeRefusal(r, err, "Failed to update payment method")
		return
	}

	// Pre-flight: resolve the rail read-only so misconfiguration surfaces as
	// 503 immediately (the intent handler re-resolves at execution time).
	_, providerKey, ok, err := subscriptions.NMIClientForExistingSubscription(ctx, r.State.CollectionResolver, subscription)
	if err != nil {
		log.WithError(err).WithField("subscription_id", subscription.ID).Error("failed to resolve NMI PSP for subscription")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to resolve payment rail")
		return
	}
	if !ok {
		log.WithFields(log.Fields{"rail": subscription.Rail, "psp": providerKey}).Error("NMI client not found for subscription PSP")
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
		switch {
		case errors.Is(err, subscriptions.ErrPaymentMethodProviderAccountMismatch):
			// The durable seam re-read the target under its row lock and found
			// it attributed to another PSP (#657): a refusal, not a fault.
			writePaymentMethodPSPMismatch(r)
		case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound):
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
		default:
			writeRefusal(r, err, "Failed to update payment method")
		}
		return
	}
	switch {
	case out.Done:
		log.WithFields(log.Fields{"subscription_id": subscription.ID, "rail_subscription": subscription.RailSubscriptionID, "old_payment_method_id": oldPaymentMethodID, "new_payment_method_id": paymentMethodID, "user_id": targetUserID}).Info("Subscription payment method updated successfully")
		r.SuccessJSON(map[string]any{"success": true, "message": "Payment method updated successfully", "subscription_id": openrails.SubscriptionID(subscription.ID), "payment_method_id": openrails.PaymentMethodID(paymentMethodID)})
	case out.Terminal && out.Code == intents.EvidenceCodePSPMismatch:
		log.WithFields(log.Fields{"subscription_id": subscription.ID, "payment_method_id": paymentMethodID, "reason": out.Reason}).Info("Payment-source update refused: provider-account mismatch at execution")
		writePaymentMethodPSPMismatch(r)
	case out.Terminal && out.Code == subscriptions.ErrPaymentMethodNotPSPVaulted.Code:
		writeRefusal(r, subscriptions.ErrPaymentMethodNotPSPVaulted, "Failed to update payment method")
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

// writePaymentMethodPSPMismatch renders openrails.CodePaymentMethodPSPMismatch:
// the named method was vaulted by another provider account than the
// subscription's; nothing reached the provider. Same answer at the HTTP
// pre-check and at the durable seam (#657).
func writePaymentMethodPSPMismatch(r *httprequest.Request) {
	r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, openrails.CodePaymentMethodPSPMismatch,
		"This payment method belongs to a different provider account than the subscription. Add the card again on the subscription's provider."))
}
