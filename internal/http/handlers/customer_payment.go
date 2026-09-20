package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
)

func customerActionPayer(r *httprequest.Request) (identity.CustomerID, bool) {
	p, ok := middleware.PrincipalFromRequest(r)
	if !ok || p.InvokerScoped() || p.CredentialClass != billingauth.CredentialClassUserSession {
		r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeAuthorization, "customer_action_required", "verified customer action required"))
		return identity.CustomerID{}, false
	}
	return selfAccountPayer(r)
}
func customerPaymentKey(r *httprequest.Request) (string, bool) {
	key := strings.TrimSpace(r.Request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 255 {
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_param", "Idempotency-Key (1–255 bytes) is required"))
		return "", false
	}
	if cardguard.ContainsPAN(key) {
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_param", "Idempotency-Key must not contain card data"))
		return "", false
	}
	return key, true
}
func PayMyInvoiceNow(r *httprequest.Request) {
	payer, ok := customerActionPayer(r)
	if !ok {
		return
	}
	key, ok := customerPaymentKey(r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.Param("id"))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid invoice id")
		return
	}
	var body openrails.PayInvoiceNowRequest
	if !r.BindJSON(&body) {
		return
	}
	if body.PaymentMethodID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "payment_method_id required")
		return
	}
	body.InvoiceID, body.IdempotencyKey = id, key
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	result, err := svc.PayInvoiceNow(r.Request.Context(), payer, body)
	if err != nil {
		customerPaymentError(r, err)
		return
	}
	status := http.StatusOK
	if result.Operation.Unresolved() {
		status = http.StatusAccepted
	}
	r.JSON(status, result)
}
func RetryMySubscriptionNow(r *httprequest.Request) {
	payer, ok := customerActionPayer(r)
	if !ok {
		return
	}
	key, ok := customerPaymentKey(r)
	if !ok {
		return
	}
	id, err := openrails.ParseSubscriptionID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription id")
		return
	}
	var body openrails.RetrySubscriptionNowRequest
	if !r.BindJSON(&body) {
		return
	}
	if body.PaymentMethodID != nil && body.PaymentMethodID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment_method_id")
		return
	}
	body.SubscriptionID, body.IdempotencyKey = id, key
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	result, err := svc.RetrySubscriptionNow(r.Request.Context(), payer, body)
	if err != nil {
		customerPaymentError(r, err)
		return
	}
	status := http.StatusOK
	if result.Operation.Unresolved() {
		status = http.StatusAccepted
	}
	r.JSON(status, result)
}
func customerPaymentError(r *httprequest.Request, err error) {
	var refusal *billingservice.CustomerPaymentRefusal
	if errors.As(err, &refusal) {
		apiError := paymentRefusalError(refusal.Code)
		if apiError.Metadata == nil {
			apiError.Metadata = map[string]any{}
		}
		apiError.Metadata["operation_id"] = refusal.OperationID.String()
		r.APIError(apiError)
		return
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, api.CodeResourceNotFound, "billing resource not found"))
	case errors.Is(err, money.ErrInvoiceRetryIdempotencyConflict), errors.Is(err, intents.ErrRebillKeyConflict):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "payment_idempotency_conflict", "key belongs to another payment request"))
	case errors.Is(err, money.ErrInvoiceRetryInProgress), errors.Is(err, money.ErrInvoiceRetryOutcomeUnknown), errors.Is(err, intents.ErrRebillInProgress):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "payment_in_progress", "a payment is already unresolved; read this resource before retrying"))
	case errors.Is(err, money.ErrCustomerPaymentUnsupported), errors.Is(err, intents.ErrRebillUnsupported):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "customer_payment_unsupported", "customer-present payment is unsupported for this rail or method"))
	case errors.Is(err, money.ErrInvoiceNotRetryable), errors.Is(err, intents.ErrRebillNotRetryable):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "payment_not_retryable", "resource is not payable now"))
	case errors.Is(err, money.ErrCollectionPaymentMethodInvalid), errors.Is(err, money.ErrCollectionPaymentMethodRequired):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_payment_method", "payment method is not eligible"))
	default:
		r.InternalError("customer payment failed", err)
	}
}
