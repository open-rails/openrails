package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	billingservice "github.com/open-rails/openrails/internal/service"
)

func customerActionPayer(r *httprequest.Request) (identity.CustomerID, bool) {
	if !customerInitiatedChargeAllowed(r) {
		return identity.CustomerID{}, false
	}
	return selfAccountPayer(r)
}

// customerInitiatedChargeAllowed refuses a customer-initiated charge unless
// the customer acts in person: not an invoker, an application or a key
// automating the account.
func customerInitiatedChargeAllowed(r *httprequest.Request) bool {
	scope, ok := r.CustomerScope()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return false
	}
	if !scope.Present() {
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, billing.CodeCustomerActionRequired, "verified customer action required"))
		return false
	}
	return true
}

func paymentActionKey(r *httprequest.Request) (string, bool) {
	key := strings.TrimSpace(r.Request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 255 {
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, billing.CodeInvalidParam, "Idempotency-Key (1–255 bytes) is required"))
		return "", false
	}
	if cardguard.ContainsPAN(key) {
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, billing.CodeInvalidParam, "Idempotency-Key must not contain card data"))
		return "", false
	}
	return key, true
}
func RetryMySubscriptionNow(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	payer, ok := customerActionPayer(r)
	if !ok {
		return
	}
	key, ok := paymentActionKey(r)
	if !ok {
		return
	}
	id, err := billing.ParseSubscriptionID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid subscription id")
		return
	}
	var body billing.RetrySubscriptionNowParams
	if !r.BindJSON(&body) {
		return
	}
	if body.PaymentMethodID != nil && body.PaymentMethodID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid payment_method_id")
		return
	}
	if refuseBlockedCardAttempt(r, payer.String()) {
		return
	}
	svc, err := billingservice.New(r.Log, r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	ctx := intents.WithOrderCaptchaSubjects(r.Request.Context(), middleware.SubjectKeysFromContext(r.Request.Context()))
	result, err := svc.RetrySubscriptionNow(ctx, payer, id, key, body, verifiedPrincipal(r))
	if err != nil {
		var refusal *billingservice.CustomerPaymentRefusal
		if errors.As(err, &refusal) {
			noteCardDecline(r, payer.String())
		}
		customerPaymentError(r, err)
		return
	}
	status := http.StatusOK
	if result.Status == "processing" {
		status = http.StatusAccepted
	}
	r.JSON(status, result)
}
func customerPaymentError(r *httprequest.Request, err error) {
	var refusal *billingservice.CustomerPaymentRefusal
	if errors.As(err, &refusal) {
		r.APIError(paymentRefusalError(refusal.Code))
		return
	}
	switch {
	case errors.Is(err, money.ErrUsageRecoveryHeld):
		r.ErrorCode(billing.CodeServiceUnavailable, "this order needs its provider payment history to be verified")
	case errors.Is(err, money.ErrCustomerSessionRequired):
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, billing.CodeCustomerActionRequired, "verified customer action required"))
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, subscriptions.ErrSubscriptionNotFound):
		r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, api.CodeResourceNotFound, "billing resource not found"))
	case errors.Is(err, intents.ErrRebillKeyConflict):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, billing.CodePaymentIdempotencyConflict, "key belongs to another payment request"))
	case errors.Is(err, intents.ErrRebillInProgress):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, billing.CodePaymentInProgress, "a payment is already unresolved; read this resource before retrying"))
	case errors.Is(err, money.ErrCustomerPaymentUnsupported), errors.Is(err, intents.ErrRebillUnsupported):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, billing.CodeCustomerPaymentUnsupported, "customer-present payment is unsupported for this rail or method"))
	case errors.Is(err, intents.ErrRetryNeedsNewMethod):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, billing.CodePaymentNotRetryable, "this card is not charged again for this renewal; set another payment method on the subscription"))
	case errors.Is(err, intents.ErrRebillNotRetryable):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, billing.CodePaymentNotRetryable, "resource is not payable now"))
	case errors.Is(err, money.ErrDefaultPaymentMethodInvalid), errors.Is(err, money.ErrDefaultPaymentMethodRequired), errors.Is(err, charge.ErrAgreementRequired):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, billing.CodeInvalidPaymentMethod, "payment method is not eligible"))
	default:
		r.InternalError("customer payment failed", err)
	}
}
