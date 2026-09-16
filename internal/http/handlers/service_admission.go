package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
	log "github.com/sirupsen/logrus"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/api"
	billingidentity "github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// maxAdmitBatchItems bounds one /v1/merchant/admissions request (#335).
const maxAdmitBatchItems = 1000

type serviceAdmitRequest = openrails.AdmitRequest

// admitInputFromRequest maps one admission item onto the service-facade input.
func admitInputFromRequest(req serviceAdmitRequest, payer billingidentity.CustomerID) billingservice.AdmitInput {
	in := billingservice.AdmitInput{
		CustomerID:      payer,
		Invoker:         strings.TrimSpace(req.Invoker),
		InvokerType:     req.InvokerType,
		TrustLevel:      strings.TrimSpace(req.TrustLevel),
		Resource:        req.Resource,
		Currency:        req.Currency,
		EstimatedAmount: req.EstimatedAmount,

		AccrualRateDeltaPerHour: req.AccrualRateDeltaPerHour,
		Source:                  req.Source,
		SourceID:                req.RequestID,
		Roles:                   req.Roles,
	}
	if req.ExpiresAt != nil {
		in.ExpiresAt = *req.ExpiresAt
	}
	return in
}

// admitVerdictStatus maps an admission result onto the per-item HTTP-equivalent
// status returned by the batch route.
func admitVerdictStatus(res *billingservice.AdmitResult) int {
	if res.Allowed {
		return http.StatusOK
	}
	switch res.BlockedBy {
	case "abuse":
		return http.StatusTooManyRequests
	case "money":
		return http.StatusPaymentRequired
	default:
		return http.StatusForbidden
	}
}

type serviceAdmitBatchRequest struct {
	Items []serviceAdmitRequest `json:"items"`
}

// serviceAdmitVerdict is one per-item batch outcome (#335). Status is the
// HTTP-equivalent status for this item; Result carries the full admission
// decision when one was reached.
type serviceAdmitVerdict struct {
	Status int                         `json:"status"`
	Error  *api.ErrorDetails           `json:"error,omitempty"`
	Result *billingservice.AdmitResult `json:"result,omitempty"`
}

func admitFailure(status int, message, param string) serviceAdmitVerdict {
	details := api.SimpleErrorResponse(status, message).Error
	if param != "" {
		details.Param = &param
	}
	return serviceAdmitVerdict{Status: status, Error: &details}
}

// serviceAdmitBatchVerdicts runs the per-item admission loop with FULL per-item
// isolation: a bad payer id, scope denial, deny, or backend error on one item
// never fails the others. allows gates each item's payer against the service
// API key's customer scope; admit is the (injected) single-admit core.
//
// Follow-up (#335): each item currently runs the full single-admit path. An
// obvious optimization is batching shared payer reads inside one transaction.
func serviceAdmitBatchVerdicts(
	ctx context.Context,
	items []serviceAdmitRequest,
	allows func(billingidentity.CustomerID) bool,
	admit func(context.Context, billingservice.AdmitInput) (*billingservice.AdmitResult, error),
) []serviceAdmitVerdict {
	out := make([]serviceAdmitVerdict, len(items))
	for i, item := range items {
		if item.EstimatedAmount < 0 {
			out[i] = admitFailure(http.StatusBadRequest, "estimated_amount must be >= 0", "estimated_amount")
			continue
		}
		payer, err := parseServiceCustomerID(item.CustomerID)
		if err != nil || payer == nil {
			out[i] = admitFailure(http.StatusBadRequest, "customer_id required", "customer_id")
			continue
		}
		var invalid *spendgate.ValidationError
		if err := spendgate.ValidateRequest(item.RequestID, item.EstimatedAmount, item.AccrualRateDeltaPerHour); errors.As(err, &invalid) {
			out[i] = admitFailure(http.StatusBadRequest, invalid.Message, invalid.Param)
			continue
		}
		if !allows(*payer) {
			out[i] = admitFailure(http.StatusForbidden, "service_credential_customer_scope_denied", "")
			continue
		}
		res, err := admit(ctx, admitInputFromRequest(item, *payer))
		switch {
		case errors.As(err, &invalid):
			out[i] = admitFailure(http.StatusBadRequest, invalid.Message, invalid.Param)
			continue
		case errors.Is(err, billingservice.ErrIdempotencyKeyReused):
			details := api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, api.CodeIdempotencyKeyReused, err.Error()).ToResponse().Error
			out[i] = serviceAdmitVerdict{Status: http.StatusConflict, Error: &details}
			continue
		case errors.Is(err, billingservice.ErrHoldDeadlineRequired):
			out[i] = admitFailure(http.StatusBadRequest, "expires_at required when estimated_amount places a hold", "expires_at")
			continue
		case errors.Is(err, billingservice.ErrHoldDeadlinePassed):
			out[i] = admitFailure(http.StatusBadRequest, "expires_at already passed", "expires_at")
			continue
		}
		if err != nil {
			// upstream#1627: the wire string stays stable and non-leaky (it reaches the
			// host's tenant boundary), but the CAUSE must not be discarded — a 500
			// whose only content is the constant "admission check failed" cost a
			// cross-stack bisect to attribute. Operators read this log.
			log.WithError(err).WithFields(log.Fields{
				"customer_id": item.CustomerID,
				"invoker":     item.Invoker,
				"request_id":  item.RequestID,
				"source":      item.Source,
			}).Error("admission check failed")
			out[i] = admitFailure(http.StatusInternalServerError, "admission check failed", "")
			continue
		}
		out[i] = serviceAdmitVerdict{Status: admitVerdictStatus(res), Result: res}
	}
	return out
}

// ServiceAdmitBatch is the cross-payer batch admission endpoint (#335): one
// request carries N admit items (mixed payers); the response carries N
// positional verdicts with the same semantics as a single admission per item.
// The batch itself always answers 200 — per-item denial/errors live in the
// items, so cold payers conflating admits collapse N hops into one without one
// broke payer poisoning the flight.
func ServiceAdmitBatch(r *httprequest.Request) {
	var req serviceAdmitBatchRequest
	if !r.BindJSON(&req) {
		return
	}
	if len(req.Items) == 0 {
		r.ErrorJSON(http.StatusBadRequest, "items required")
		return
	}
	if len(req.Items) > maxAdmitBatchItems {
		r.ErrorJSON(http.StatusBadRequest, "too many items")
		return
	}
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	verdicts := serviceAdmitBatchVerdicts(
		r.Request.Context(),
		req.Items,
		func(ts billingidentity.CustomerID) bool { return serviceCustomerScopeAllows(r, ts) },
		svc.Admit,
	)
	for i := range verdicts {
		if verdicts[i].Error != nil {
			verdicts[i].Error.RequestID = r.RequestID()
		}
	}
	r.JSON(http.StatusOK, map[string]any{"items": verdicts})
}

// ServiceGetTrustLevel returns the payer's host-assigned trust level for one
// currency. customer_id is a query param; the merchant is
// pinned from the API key. Empty trust_level means the host has not assigned a level
// (caller treats it as the lowest/default). Operator API key, credits:read.
func ServiceGetTrustLevel(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Query("currency"))
	if !ok {
		return
	}
	if err := moneyutil.ValidateCurrency(currency); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	trustLevel, err := svc.GetTrustLevel(r.Request.Context(), *payer, currency)
	if err != nil {
		r.InternalError("trust level lookup failed", err)
		return
	}
	r.SuccessJSON(map[string]any{"currency": currency, "trust_level": trustLevel})
}

type serviceReportWastedSpendRequest = openrails.WastedSpendReport

// ServiceReportWastedSpend records host-reported WASTED $ (#497): delegated
// invokers accrue toward their flat cutoff; direct payer credentials use
// trust-level grace and then normal ledger charging. Operator API key,
// credits:write.
func ServiceReportWastedSpend(r *httprequest.Request) {
	var req serviceReportWastedSpendRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.Amount < 0 {
		r.ErrorJSON(http.StatusBadRequest, "amount must be >= 0")
		return
	}
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	if strings.TrimSpace(req.Source) == "" || strings.TrimSpace(req.SourceID) == "" {
		r.ErrorJSON(http.StatusBadRequest, "source and source_id required")
		return
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	res, err := svc.ReportWastedSpend(r.Request.Context(), billingservice.WastedSpendInput{
		CustomerID:  *payer,
		Invoker:     strings.TrimSpace(req.Invoker),
		InvokerType: req.InvokerType,
		Currency:    currency,
		Amount:      req.Amount,
		Source:      req.Source,
		SourceID:    req.SourceID,
		Reason:      req.Reason,
	})
	if err != nil {
		if serviceIdempotencyConflict(r, err) {
			return
		}
		r.InternalError("report wasted spend failed", err)
		return
	}
	r.JSON(http.StatusOK, res)
}

type serviceCreditLimitRequest = openrails.CreditLimitRequest

// ServiceSetCreditLimit sets the admin/operator arrears credit line for a payer
// (#489): under billing_mode=arrears the balance may go NEGATIVE up to the limit;
// AdmitHold denies insufficient_credit when a new hold would exceed it. 0 = off.
// Merchant-admin gated at the route (`merchant:customer-settings:update`) - NOT self-serve.
func ServiceSetCreditLimit(r *httprequest.Request) {
	var req serviceCreditLimitRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.CreditLimitAmount < 0 {
		r.ErrorJSON(http.StatusBadRequest, "credit_limit_amount must be >= 0")
		return
	}
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	if err := svc.SetCreditLimit(r.Request.Context(), *payer, currency, req.CreditLimitAmount); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSONMessage("ok")
}

// ServiceGetCreditLimit returns the admin-set arrears credit line for a payer
// (#489). customer_id is a query param; the merchant is pinned from the API key.
// Operator API key, credits:read.
func ServiceGetCreditLimit(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Query("currency"))
	if !ok {
		return
	}
	v, err := svc.GetCreditLimit(r.Request.Context(), *payer, currency)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(openrails.CreditLimitRequest{CustomerID: payer.String(), Currency: currency, CreditLimitAmount: v})
}

// ServiceGetMerchantSettings returns the complete declarative policy document.
func ServiceGetMerchantSettings(r *httprequest.Request) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	settings, err := svc.GetMerchantSettings(r.Request.Context())
	if err != nil {
		r.InternalError("get merchant settings failed", err)
		return
	}
	r.SuccessJSON(settings)
}

// ServiceSetMerchantSettings atomically replaces the declarative policy document.
func ServiceSetMerchantSettings(r *httprequest.Request) {
	var settings *openrails.MerchantSettings
	decoder := json.NewDecoder(r.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			r.ErrorJSON(http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var extra any
	if settings == nil || decoder.Decode(&extra) != io.EOF {
		r.ErrorJSON(http.StatusBadRequest, "one settings document required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	if err := svc.SetMerchantSettings(r.Request.Context(), *settings); err != nil {
		if errors.Is(err, billingservice.ErrInvalidMerchantSettings) {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		r.InternalError("set merchant settings failed", err)
		return
	}
	r.SuccessJSONMessage("ok")
}
