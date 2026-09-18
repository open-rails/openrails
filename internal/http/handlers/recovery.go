package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

// Customer payment recovery (#809). The self routes act for the authenticated
// payer; the merchant routes act for the customer a host names, having
// authenticated them itself. Both answer identically: 200 with the result
// once the attempt is terminal, 202 with the unresolved operation, 402
// card_declined for a recorded provider refusal, and a coded 409 when the
// engine refuses to attempt at all.

type payInvoiceNowBody struct {
	PaymentMethodID openrails.PaymentMethodID `json:"payment_method_id"`
}

type retrySubscriptionNowBody struct {
	PaymentMethodID *openrails.PaymentMethodID `json:"payment_method_id,omitempty"`
}

func idempotencyKey(r *httprequest.Request) (string, bool) {
	key := strings.TrimSpace(r.Request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 255 {
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_param", "Idempotency-Key (1–255 bytes) is required").WithParam("Idempotency-Key"))
		return "", false
	}
	return key, true
}

func PayMyInvoiceNow(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	payInvoiceNow(r, payer)
}

func PayCustomerInvoiceNow(r *httprequest.Request) {
	customer, err := openrails.ParseCustomerID(r.Param("customer_id"))
	if err != nil || customer.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	payInvoiceNow(r, identity.CustomerID(customer))
}

func payInvoiceNow(r *httprequest.Request, payer identity.CustomerID) {
	invoiceID, err := uuid.Parse(r.Param("id"))
	if err != nil || invoiceID == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid invoice id")
		return
	}
	var body payInvoiceNowBody
	if !r.BindJSON(&body) {
		return
	}
	if body.PaymentMethodID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "payment_method_id is required")
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	result, err := svc.PayInvoiceNow(r.Request.Context(), payer, openrails.PayInvoiceNowRequest{InvoiceID: invoiceID, PaymentMethodID: body.PaymentMethodID, IdempotencyKey: key})
	if err != nil {
		writeRecoveryError(r, err)
		return
	}
	r.JSON(recoveryStatus(result.Operation), result)
}

func RetryMySubscriptionNow(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	retrySubscriptionNow(r, payer)
}

func RetryCustomerSubscriptionNow(r *httprequest.Request) {
	customer, err := openrails.ParseCustomerID(r.Param("customer_id"))
	if err != nil || customer.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	retrySubscriptionNow(r, identity.CustomerID(customer))
}

func retrySubscriptionNow(r *httprequest.Request, payer identity.CustomerID) {
	subscriptionID, err := openrails.ParseSubscriptionID(r.Param("id"))
	if err != nil || subscriptionID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}
	// The body is optional: retry-now charges the subscription's current
	// method unless the caller names it explicitly.
	var body retrySubscriptionNowBody
	raw, err := io.ReadAll(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request body")
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid request body")
			return
		}
	}
	if body.PaymentMethodID != nil && body.PaymentMethodID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment_method_id")
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	result, err := svc.RetrySubscriptionNow(r.Request.Context(), payer, openrails.RetrySubscriptionNowRequest{SubscriptionID: subscriptionID, PaymentMethodID: body.PaymentMethodID, IdempotencyKey: key})
	if err != nil {
		writeRecoveryError(r, err)
		return
	}
	r.JSON(recoveryStatus(result.Operation), result)
}

func recoveryStatus(operation openrails.PaymentOperation) int {
	if operation.Unresolved() {
		return http.StatusAccepted
	}
	return http.StatusOK
}

var recoveryRefusals = []struct {
	err    error
	status int
	code   string
}{
	{money.ErrPaymentRecoveryRailUnsupported, http.StatusConflict, openrails.CodePaymentRecoveryRailUnsupported},
	{billingservice.ErrSubscriptionNotRetryable, http.StatusConflict, openrails.CodeSubscriptionNotRetryable},
	{billingservice.ErrSubscriptionRetryInProgress, http.StatusConflict, openrails.CodeSubscriptionRetryInProgress},
	{billingservice.ErrSubscriptionRetryOutcomeUnknown, http.StatusConflict, openrails.CodeSubscriptionRetryOutcomeUnknown},
	{money.ErrInvoiceNotRetryable, http.StatusConflict, openrails.CodeInvoiceNotRetryable},
	{money.ErrInvoiceRetryInProgress, http.StatusConflict, openrails.CodeInvoiceRetryInProgress},
	{money.ErrInvoiceRetryOutcomeUnknown, http.StatusConflict, openrails.CodeInvoiceRetryOutcomeUnknown},
	{money.ErrInvoiceRetryIdempotencyConflict, http.StatusConflict, openrails.CodeInvoiceRetryIdempotencyConflict},
	{money.ErrCollectionPaymentMethodInvalid, http.StatusBadRequest, openrails.CodeCollectionPaymentMethodInvalid},
	{money.ErrCollectionPaymentMethodRequired, http.StatusBadRequest, openrails.CodeCollectionPaymentMethodRequired},
}

// writeRecoveryError renders a recovery refusal. A recorded decline is the
// shared 402 card_declined contract with the attempt in its metadata; a
// customer can never learn about another customer's resource (404).
func writeRecoveryError(r *httprequest.Request, err error) {
	var invoiceDeclined *billingservice.InvoiceDeclined
	if errors.As(err, &invoiceDeclined) {
		result := invoiceDeclined.Result
		refusal := refusalForReason(derefStr(result.Attempt.FailureReason), derefStr(result.Attempt.FailureCode))
		refusal.Metadata["attempt_id"] = result.Attempt.ID.String()
		refusal.Metadata["invoice_id"] = result.Invoice.ID.String()
		refusal.Metadata["operation_id"] = result.Operation.ID.String()
		refusal.Metadata["replayed"] = result.Replayed
		if result.Invoice.Recovery != nil {
			refusal.Metadata["retryable"] = result.Invoice.Recovery.Retryable
			refusal.Metadata["attempt_count"] = result.Invoice.Recovery.AttemptCount
			if result.Invoice.Recovery.NextAttemptAt != nil {
				refusal.Metadata["next_attempt_at"] = result.Invoice.Recovery.NextAttemptAt.UTC()
			}
		}
		r.APIError(refusal)
		return
	}
	var subscriptionDeclined *billingservice.SubscriptionDeclined
	if errors.As(err, &subscriptionDeclined) {
		result := subscriptionDeclined.Result
		refusal := refusalForReason(payments.NormalizeFailureReason(result.Subscription.Rail, subscriptionDeclined.FailureCode), subscriptionDeclined.FailureCode)
		refusal.Metadata["subscription_id"] = result.Subscription.ID.String()
		refusal.Metadata["operation_id"] = result.Operation.ID.String()
		refusal.Metadata["subscription_status"] = result.Subscription.Status
		refusal.Metadata["replayed"] = result.Replayed
		if result.Subscription.Recovery != nil {
			refusal.Metadata["retryable"] = result.Subscription.Recovery.Retryable
			refusal.Metadata["attempt_count"] = result.Subscription.Recovery.AttemptCount
			if result.Subscription.Recovery.NextAttemptAt != nil {
				refusal.Metadata["next_attempt_at"] = result.Subscription.Recovery.NextAttemptAt.UTC()
			}
		}
		r.APIError(refusal)
		return
	}
	if errors.Is(err, billingservice.ErrSubscriptionNotFound) || db.IsNotFound(err) {
		r.ErrorJSON(http.StatusNotFound, "not found")
		return
	}
	for _, refusal := range recoveryRefusals {
		if errors.Is(err, refusal.err) {
			r.APIError(api.NewAPIError(refusal.status, api.ErrorTypeInvalidRequest, refusal.code, err.Error()))
			return
		}
	}
	log.WithError(err).Warn("customer payment recovery failed")
	r.ErrorJSON(http.StatusInternalServerError, "payment recovery failed")
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
