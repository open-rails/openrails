package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/writeposture"
	log "github.com/sirupsen/logrus"
)

type UpdateSubscriptionPaymentMethodBody = billing.SetSubscriptionPaymentMethodParams

func SetSubscriptionPaymentMethod(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorCode(billing.CodeAuthenticationRequired, "Authentication required")
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
		r.ErrorCode(billing.CodeInvalidParam, "subscription ID required")
		return
	}

	typedSubscriptionID, err := billing.ParseSubscriptionID(subscriptionIDStr)
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "Invalid subscription ID format")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()

	var req UpdateSubscriptionPaymentMethodBody
	if !r.BindJSON(&req) {
		return
	}

	if req.PaymentMethodID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "Invalid payment_method_id format")
		return
	}
	paymentMethodID := req.PaymentMethodID.UUID()

	ctx, cancel := r.Budget(15 * time.Second)
	defer cancel()

	subscription, err := r.State.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			r.ErrorCode(billing.CodeResourceNotFound, "Subscription not found")
			return
		}
		log.WithError(err).WithField("subscription_id", subscriptionID).Error("Failed to get subscription")
		r.ErrorCode(billing.CodeInternalError, "Failed to retrieve subscription")
		return
	}

	targetUserID := subscription.CustomerID.String()
	if enforceOwnership && targetUserID != authenticatedUserID {
		r.ErrorCode(billing.CodeResourceNotFound, "Subscription not found")
		return
	}

	if subscription.CollectionPolicy == models.CollectionPolicyEngine {
		origin := intents.OriginUser
		if !enforceOwnership {
			origin = intents.OriginAdmin
		}
		verify := func(ctx context.Context, vault, billingID, order string) (string, error) {
			if blocked, reason := intents.GateExecution(ctx, writeposture.View{Config: r.State.Config, DB: r.State.DB}, subscription.MerchantID, origin); blocked {
				return "", apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "the recurring verification cannot be sent: "+reason)
			}
			client, _, ok, err := subscriptions.NMIClientForExistingSubscription(ctx, r.State.CollectionResolver, subscription)
			if err != nil || !ok {
				return "", fmt.Errorf("resolve the subscription's NMI account: %w", err)
			}
			ref, err := client.VerifyStoredCredential(ctx, vault, billingID, order, true)
			var refusal *nmi.CustomerVaultError
			if errors.As(err, &refusal) && !nmi.UncertainResponseCode(refusal.ResponseCode) {
				return "", &paymentmethods.PaymentMethodError{Err: err, LocalizationID: nmidirect.FailureCode(refusal), Rail: "nmi"}
			}
			return ref, err
		}
		if err := r.State.SubscriptionLifecycleService.UpdateEnginePaymentMethod(ctx, subscription.ID, subscription.CustomerID, paymentMethodID, verify); err != nil {
			var refused *paymentmethods.PaymentMethodError
			if errors.As(err, &refused) {
				writePaymentMethodError(r, refused)
				return
			}
			writeRefusal(r, err, "Failed to select payment method")
			return
		}
		writeUpdatedSubscription(r, authenticatedUserID, enforceOwnership, subscription.ID)
		return
	}
	if !rails.IsNMI(subscription.Rail) {
		r.ErrorCode(billing.CodeInvalidParam, "Only NMI-backed subscriptions can have their payment method updated")
		return
	}

	if subscription.Status != models.StatusActive && subscription.Status != models.StatusPastDue && subscription.Status != models.StatusAwaitingMethod {
		r.ErrorCode(billing.CodeInvalidParam, "Cannot update payment method for canceled subscriptions")
		return
	}

	paymentMethod, err := r.State.PaymentMethodService.ValidatePaymentMethodOperation(ctx, paymentMethodID, targetUserID)
	if err != nil {
		switch {
		case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound):
			r.ErrorCode(billing.CodeResourceNotFound, "Payment method not found")
			return
		case errors.Is(err, paymentmethods.ErrPaymentMethodAccessDenied):
			r.ErrorCode(billing.CodeResourceNotFound, "Payment method not found")
			return
		default:
			log.WithError(err).WithFields(log.Fields{"payment_method_id": paymentMethodID, "user_id": targetUserID}).Error("Failed to validate payment method ownership")
			r.ErrorCode(billing.CodeInternalError, "Failed to validate payment method")
			return
		}
	}

	if !rails.IsNMI(paymentMethod.Rail) {
		r.ErrorCode(billing.CodeInvalidParam, "Only NMI-backed payment methods can be used")
		return
	}
	if !rails.SameRail(paymentMethod.Rail, subscription.Rail) {
		r.ErrorCode(billing.CodeInvalidParam, "Payment method belongs to a different payment provider")
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
		r.ErrorCode(billing.CodeInternalError, "Failed to resolve payment rail")
		return
	}
	if !ok {
		log.WithFields(log.Fields{"rail": subscription.Rail, "psp": providerKey}).Error("NMI client not found for subscription PSP")
		r.ErrorCode(billing.CodeServiceUnavailable, "Payment rail not available")
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
			r.ErrorCode(billing.CodeResourceNotFound, "Payment method not found")
		default:
			writeRefusal(r, err, "Failed to update payment method")
		}
		return
	}
	switch {
	case out.Done:
		log.WithFields(log.Fields{"subscription_id": subscription.ID, "rail_subscription": subscription.RailSubscriptionID, "old_payment_method_id": oldPaymentMethodID, "new_payment_method_id": paymentMethodID, "user_id": targetUserID}).Info("Subscription payment method updated successfully")
		writeUpdatedSubscription(r, authenticatedUserID, enforceOwnership, subscription.ID)
	case out.Terminal && out.Code == intents.EvidenceCodePSPMismatch:
		log.WithFields(log.Fields{"subscription_id": subscription.ID, "payment_method_id": paymentMethodID, "reason": out.Reason}).Info("Payment-source update refused: provider-account mismatch at execution")
		writePaymentMethodPSPMismatch(r)
	case out.Terminal && out.Code == subscriptions.ErrPaymentMethodNotPSPVaulted.Code:
		writeRefusal(r, subscriptions.ErrPaymentMethodNotPSPVaulted, "Failed to update payment method")
	case out.Terminal:
		log.WithFields(log.Fields{"subscription_id": subscription.ID, "rail_subscription": subscription.RailSubscriptionID, "new_vault_id": paymentMethod.RailCustomerRef, "payment_method_id": paymentMethod.ID, "reason": out.Reason}).Error("Failed to update subscription payment source with NMI")
		r.ErrorCode("payment_method_update_failed", "")
	default:
		// Ambiguous/parked: neither success nor decline — the durable intent
		// finishes out-of-band and a retried request maps onto the SAME intent.
		log.WithFields(log.Fields{"subscription_id": subscription.ID, "payment_method_id": paymentMethodID, "reason": out.Reason}).Warn("Payment-source update unresolved inline; intent ledger will converge")
		r.ErrorCode(billing.CodeResourceConflict, intents.ErrPaymentSourceUpdateProcessing.Error())
	}
}

// writeUpdatedSubscription answers the subscription after its payment method
// changed, as the caller's audience reads it.
func writeUpdatedSubscription(r *httprequest.Request, userID string, customer bool, id uuid.UUID) {
	if customer {
		writeMySubscription(r, userID, id, nil)
		return
	}
	writeMerchantSubscription(r, id)
}

// writePaymentMethodPSPMismatch renders billing.CodePaymentMethodPSPMismatch:
// the named method was vaulted by another provider account than the
// subscription's; nothing reached the provider. Same answer at the HTTP
// pre-check and at the durable seam (#657).
func writePaymentMethodPSPMismatch(r *httprequest.Request) {
	r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, billing.CodePaymentMethodPSPMismatch,
		"This payment method belongs to a different provider account than the subscription. Add the card again on the subscription's provider."))
}
