package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	billingidentity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// maxAdmitBatchItems bounds one admission batch.
const maxAdmitBatchItems = 1000

func admitInput(p billing.AdmitParams) billingservice.AdmitInput {
	in := billingservice.AdmitInput{
		CustomerID:              p.CustomerID,
		Invoker:                 strings.TrimSpace(p.Invoker),
		InvokerType:             p.InvokerType,
		TrustLevel:              strings.TrimSpace(p.TrustLevel),
		Resource:                p.Resource,
		Currency:                p.Currency,
		EstimatedAmount:         p.EstimatedAmount,
		AccrualRateDeltaPerHour: p.AccrualRateDeltaPerHour,
		Source:                  p.Source,
		SourceID:                p.RequestID,
		Roles:                   p.Roles,
	}
	if p.ExpiresAt != nil {
		in.ExpiresAt = *p.ExpiresAt
	}
	return in
}

// admissionStatus is the status admitting one item alone answers.
func admissionStatus(a *billing.Admission) int {
	if a.Allowed {
		return http.StatusOK
	}
	switch *a.BlockedBy {
	case billing.AdmissionBlockedByAbuse:
		return http.StatusTooManyRequests
	case billing.AdmissionBlockedByMoney:
		return http.StatusPaymentRequired
	}
	return http.StatusForbidden
}

func admitRefusal(code, message, param string) billing.AdmissionVerdict {
	err := api.Coded(code, message)
	if param != "" {
		err = err.WithParam(param)
	}
	details := err.ToResponse().Error
	return billing.AdmissionVerdict{Status: err.HTTPStatus, Error: &details}
}

// admitVerdicts admits each item on its own: one item's bad input, scope
// denial, refusal or backend error never fails another.
func admitVerdicts(
	ctx context.Context,
	items []billing.AdmitParams,
	allows func(billing.CustomerID) bool,
	admit func(context.Context, billingservice.AdmitInput) (*billing.Admission, error),
) []billing.AdmissionVerdict {
	out := make([]billing.AdmissionVerdict, len(items))
	for i, item := range items {
		if item.CustomerID.IsZero() {
			out[i] = admitRefusal(billing.CodeInvalidParam, "customer_id required", "customer_id")
			continue
		}
		var invalid *spendgate.ValidationError
		if err := spendgate.ValidateRequest(item.RequestID, item.EstimatedAmount, item.AccrualRateDeltaPerHour); errors.As(err, &invalid) {
			out[i] = admitRefusal(billing.CodeInvalidParam, invalid.Message, invalid.Param)
			continue
		}
		if !allows(item.CustomerID) {
			out[i] = admitRefusal(billing.CodeServiceCredentialCustomerScopeDenied, "", "")
			continue
		}
		admission, err := admit(ctx, admitInput(item))
		switch {
		case err == nil:
			out[i] = billing.AdmissionVerdict{Status: admissionStatus(admission), Admission: admission}
		case errors.As(err, &invalid):
			out[i] = admitRefusal(billing.CodeInvalidParam, invalid.Message, invalid.Param)
		case errors.Is(err, billingservice.ErrIdempotencyKeyReused):
			out[i] = admitRefusal(billing.CodeIdempotencyKeyReused, err.Error(), "request_id")
		case errors.Is(err, billingservice.ErrHoldDeadlineRequired):
			out[i] = admitRefusal(billing.CodeInvalidParam, "expires_at required when estimated_amount places a hold", "expires_at")
		case errors.Is(err, billingservice.ErrHoldDeadlinePassed):
			out[i] = admitRefusal(billing.CodeInvalidParam, "expires_at already passed", "expires_at")
		default:
			var refusal interface{ ErrorCode() string }
			if errors.As(err, &refusal) {
				out[i] = admitRefusal(refusal.ErrorCode(), err.Error(), "")
				continue
			}
			// The wire stays non-leaky; the cause reaches the operator log.
			log.WithError(err).WithFields(log.Fields{"customer_id": item.CustomerID, "invoker": item.Invoker, "request_id": item.RequestID}).
				Error("admission check failed")
			out[i] = admitRefusal(billing.CodeInternalError, "admission check failed", "")
		}
	}
	return out
}

// Admit decides a batch of admissions, one verdict per item in order. The
// batch answers 200; each item carries its own outcome.
func Admit(r *httprequest.Request) {
	var params billing.AdmitBatchParams
	if !r.BindJSON(&params) {
		return
	}
	if len(params.Items) == 0 || len(params.Items) > maxAdmitBatchItems {
		r.APIError(api.Coded(billing.CodeInvalidParam, "items must hold 1 to 1000 admissions").WithParam("items"))
		return
	}
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	verdicts := admitVerdicts(r.Request.Context(), params.Items,
		func(c billing.CustomerID) bool { return serviceCustomerScopeAllows(r, c) }, svc.Admit)
	for i := range verdicts {
		if verdicts[i].Error != nil {
			verdicts[i].Error.RequestID = r.RequestID()
		}
	}
	r.JSON(http.StatusOK, billing.AdmitBatchResult{Items: verdicts})
}

// admissionParam resolves {request_id} to its customer and checks the
// credential's customer scope.
func admissionParam(r *httprequest.Request, svc *billingservice.Service) (string, bool) {
	requestID := strings.TrimSpace(r.Param("request_id"))
	customer, err := svc.AdmissionCustomer(r.Request.Context(), requestID)
	if errors.Is(err, spendgate.ErrNotFound) {
		r.APIError(api.Coded("admission_not_found", ""))
		return "", false
	}
	if err != nil {
		r.InternalError("admission lookup failed", err)
		return "", false
	}
	return requestID, requireServiceCustomerScope(r, billingidentity.CustomerID(customer))
}

// GetAdmission reads an allowed admission and its hold.
func GetAdmission(r *httprequest.Request) {
	svc, ok := billingService(r)
	if !ok {
		return
	}
	requestID, ok := admissionParam(r, svc)
	if !ok {
		return
	}
	admission, err := svc.GetAdmission(r.Request.Context(), requestID)
	if err != nil {
		writeMoneyError(r, err, "admission read failed")
		return
	}
	r.SuccessJSON(admission)
}

// CaptureAdmission settles an admitted request.
func CaptureAdmission(r *httprequest.Request) {
	var params billing.CaptureAdmissionParams
	if !r.BindJSON(&params) {
		return
	}
	if params.Amount < 0 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "amount must be nonnegative").WithParam("amount"))
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	requestID, ok := admissionParam(r, svc)
	if !ok {
		return
	}
	receipt, err := svc.CaptureAdmission(r.Request.Context(), requestID, params)
	if err != nil {
		writeMoneyError(r, err, "capture failed")
		return
	}
	r.SuccessJSON(receipt)
}

// ReleaseAdmission frees an admitted request's hold.
func ReleaseAdmission(r *httprequest.Request) {
	svc, ok := billingService(r)
	if !ok {
		return
	}
	requestID, ok := admissionParam(r, svc)
	if !ok {
		return
	}
	admission, err := svc.ReleaseAdmission(r.Request.Context(), requestID)
	if errors.Is(err, spendgate.ErrCaptured) {
		r.APIError(api.Coded("admission_captured", ""))
		return
	}
	if err != nil {
		writeMoneyError(r, err, "release failed")
		return
	}
	r.SuccessJSON(admission)
}

// ExtendAdmission moves an open hold's deadline later.
func ExtendAdmission(r *httprequest.Request) {
	var params billing.ExtendAdmissionParams
	if !r.BindJSON(&params) {
		return
	}
	if params.ExpiresAt.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "expires_at required").WithParam("expires_at"))
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	requestID, ok := admissionParam(r, svc)
	if !ok {
		return
	}
	admission, err := svc.ExtendAdmission(r.Request.Context(), requestID, params.ExpiresAt.UTC())
	switch {
	case err == nil:
		r.SuccessJSON(admission)
	case errors.Is(err, billingservice.ErrHoldNotFound):
		r.APIError(api.Coded("hold_not_found", ""))
	case errors.Is(err, billingservice.ErrHoldDeadlinePassed):
		r.APIError(api.Coded(billing.CodeInvalidParam, "expires_at already passed").WithParam("expires_at"))
	case errors.Is(err, spendgate.ErrDeadlineShortened):
		r.APIError(api.Coded(billing.CodeInvalidParam, "an extension cannot shorten the deadline").WithParam("expires_at"))
	default:
		var invalid *spendgate.ValidationError
		if errors.As(err, &invalid) {
			r.APIError(api.Coded(billing.CodeInvalidParam, invalid.Message).WithParam(invalid.Param))
			return
		}
		writeMoneyError(r, err, "extend failed")
	}
}

// ReportWastedSpend records spend a customer's invoker wasted.
func ReportWastedSpend(r *httprequest.Request) {
	var params billing.ReportWastedSpendParams
	if !r.BindJSON(&params) {
		return
	}
	if params.Amount < 0 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "amount must be nonnegative").WithParam("amount"))
		return
	}
	if params.CustomerID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "customer_id required").WithParam("customer_id"))
		return
	}
	if strings.TrimSpace(params.Source) == "" || strings.TrimSpace(params.SourceID) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "source and source_id required").WithParam("source_id"))
		return
	}
	if !requireServiceCustomerScope(r, params.CustomerID) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	report, err := svc.ReportWastedSpend(r.Request.Context(), billingservice.WastedSpendInput{
		CustomerID: params.CustomerID, Invoker: strings.TrimSpace(params.Invoker), InvokerType: params.InvokerType,
		Currency: params.Currency, Amount: params.Amount, Source: params.Source, SourceID: params.SourceID, Reason: params.Reason,
	})
	if err != nil {
		writeMoneyError(r, err, "wasted spend report failed")
		return
	}
	r.SuccessJSON(report)
}

// RecordUsageEvent records one metered usage event.
func RecordUsageEvent(r *httprequest.Request) {
	var params billing.RecordUsageParams
	if !r.BindJSON(&params) {
		return
	}
	if params.Amount < 0 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "amount must be nonnegative").WithParam("amount"))
		return
	}
	if params.CustomerID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "customer_id required").WithParam("customer_id"))
		return
	}
	if !requireServiceCustomerScope(r, params.CustomerID) {
		return
	}
	key, err := money.NewIdempotencyKey(money.UsageOperation(params.EventType), params.Source, params.SourceID)
	if err != nil {
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("source_id"))
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	in := billingservice.RecordUsageInput{
		CustomerID: params.CustomerID, Invoker: params.Invoker, Currency: params.Currency, EventType: params.EventType,
		Dimensions: params.Dimensions, Amount: params.Amount, Resource: params.Resource, Metadata: params.Metadata, Key: key,
	}
	if params.OccurredAt != nil {
		in.OccurredAt = params.OccurredAt.UTC()
	}
	event, err := svc.RecordUsage(r.Request.Context(), in)
	if err != nil {
		writeMoneyError(r, err, "usage record failed")
		return
	}
	status := http.StatusCreated
	if event.Replayed {
		status = http.StatusOK
	}
	r.JSON(status, event)
}

// GetCustomerUsage reports a customer's usage.
func GetCustomerUsage(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	getUsage(r, customer)
}
