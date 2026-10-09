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

// maxSourceIDBytes bounds a caller's source_id, as the tables holding it do.
const maxSourceIDBytes = 255

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
	if !batchItems(r, len(params.Items), billing.MaxAdmissionBatchItems) {
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

// admissionItemScope resolves one request id to its customer and checks the
// credential's customer scope; nil when the item may proceed.
func admissionItemScope(r *httprequest.Request, svc *billingservice.Service, requestID string) *api.APIError {
	if requestID == "" {
		return api.Coded(billing.CodeInvalidParam, "request_id required").WithParam("request_id")
	}
	customer, err := svc.AdmissionCustomer(r.Request.Context(), requestID)
	if errors.Is(err, spendgate.ErrNotFound) {
		return api.Coded("admission_not_found", "")
	}
	if err != nil {
		log.WithContext(r.Request.Context()).WithError(err).WithField("request_id", requestID).Error("admission lookup failed")
		return api.Coded(billing.CodeInternalError, "admission lookup failed")
	}
	if !serviceCustomerScopeAllows(r, billingidentity.CustomerID(customer)) {
		return api.Coded(billing.CodeServiceCredentialCustomerScopeDenied, "")
	}
	return nil
}

func admissionRefusal(err *api.APIError) billing.AdmissionResult {
	details := err.ToResponse().Error
	return billing.AdmissionResult{Status: err.HTTPStatus, Error: &details}
}

// admissionBatch answers one result per item, in order: each runs on its own,
// exactly as it would alone.
func admissionBatch(r *httprequest.Request, n int, run func(*billingservice.Service, int) billing.AdmissionResult) {
	if !batchItems(r, n, billing.MaxAdmissionBatchItems) || !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out := make([]billing.AdmissionResult, n)
	for i := range out {
		out[i] = run(svc, i)
		if out[i].Error != nil {
			out[i].Error.RequestID = r.RequestID()
		}
	}
	r.JSON(http.StatusOK, billing.AdmissionBatchResult{Items: out})
}

// ReleaseAdmissions frees the holds of admitted requests, one result per
// request id. Releasing a released admission answers it.
func ReleaseAdmissions(r *httprequest.Request) {
	var params billing.ReleaseAdmissionBatchParams
	if !r.BindJSON(&params) {
		return
	}
	admissionBatch(r, len(params.RequestIDs), func(svc *billingservice.Service, i int) billing.AdmissionResult {
		requestID := strings.TrimSpace(params.RequestIDs[i])
		if refusal := admissionItemScope(r, svc, requestID); refusal != nil {
			return admissionRefusal(refusal)
		}
		admission, err := svc.ReleaseAdmission(r.Request.Context(), requestID)
		switch {
		case err == nil:
			return billing.AdmissionResult{Status: http.StatusOK, Admission: admission}
		case errors.Is(err, spendgate.ErrCaptured):
			return admissionRefusal(api.Coded("admission_captured", ""))
		}
		return admissionRefusal(admissionFailure(r, err, requestID, "release failed"))
	})
}

// ExtendAdmissions moves open holds' deadlines later, one result per item.
func ExtendAdmissions(r *httprequest.Request) {
	var params billing.ExtendAdmissionBatchParams
	if !r.BindJSON(&params) {
		return
	}
	admissionBatch(r, len(params.Items), func(svc *billingservice.Service, i int) billing.AdmissionResult {
		item := params.Items[i]
		requestID := strings.TrimSpace(item.RequestID)
		if item.ExpiresAt.IsZero() {
			return admissionRefusal(api.Coded(billing.CodeInvalidParam, "expires_at required").WithParam("expires_at"))
		}
		if refusal := admissionItemScope(r, svc, requestID); refusal != nil {
			return admissionRefusal(refusal)
		}
		admission, err := svc.ExtendAdmission(r.Request.Context(), requestID, item.ExpiresAt.UTC())
		var invalid *spendgate.ValidationError
		switch {
		case err == nil:
			return billing.AdmissionResult{Status: http.StatusOK, Admission: admission}
		case errors.Is(err, billingservice.ErrHoldNotFound):
			return admissionRefusal(api.Coded("hold_not_found", ""))
		case errors.Is(err, billingservice.ErrHoldDeadlinePassed):
			return admissionRefusal(api.Coded(billing.CodeInvalidParam, "expires_at already passed").WithParam("expires_at"))
		case errors.Is(err, spendgate.ErrDeadlineShortened):
			return admissionRefusal(api.Coded(billing.CodeInvalidParam, "an extension cannot shorten the deadline").WithParam("expires_at"))
		case errors.As(err, &invalid):
			return admissionRefusal(api.Coded(billing.CodeInvalidParam, invalid.Message).WithParam(invalid.Param))
		}
		return admissionRefusal(admissionFailure(r, err, requestID, "extend failed"))
	})
}

// admissionFailure is a money refusal, or an internal error whose cause
// reaches the operator log and never the wire.
func admissionFailure(r *httprequest.Request, err error, requestID, message string) *api.APIError {
	if refusal := moneyRefusal(err); refusal != nil {
		return refusal
	}
	log.WithContext(r.Request.Context()).WithError(err).WithField("request_id", requestID).Error(message)
	return api.Coded(billing.CodeInternalError, message)
}

// ReportWastedSpend records spend customers' invokers wasted, one result per
// report in order: each is handled or refused on its own, exactly as
// reporting it alone would be.
func ReportWastedSpend(r *httprequest.Request) {
	var params billing.ReportWastedSpendBatchParams
	if !r.BindJSON(&params) {
		return
	}
	if !batchItems(r, len(params.Items), billing.MaxBatchItems) || !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out := make([]billing.WastedSpendResult, len(params.Items))
	for i, item := range params.Items {
		out[i] = reportWastedSpendItem(r, svc, item)
		if out[i].Error != nil {
			out[i].Error.RequestID = r.RequestID()
		}
	}
	r.JSON(http.StatusOK, billing.ReportWastedSpendBatchResult{Items: out})
}

func wastedSpendRefusal(err *api.APIError) billing.WastedSpendResult {
	details := err.ToResponse().Error
	return billing.WastedSpendResult{Status: err.HTTPStatus, Error: &details}
}

func reportWastedSpendItem(r *httprequest.Request, svc *billingservice.Service, params billing.ReportWastedSpendParams) billing.WastedSpendResult {
	switch {
	case params.Amount < 0:
		return wastedSpendRefusal(api.Coded(billing.CodeInvalidParam, "amount must be nonnegative").WithParam("amount"))
	case params.CustomerID.IsZero():
		return wastedSpendRefusal(api.Coded(billing.CodeInvalidParam, "customer_id required").WithParam("customer_id"))
	case strings.TrimSpace(params.Source) == "" || strings.TrimSpace(params.SourceID) == "" || len(params.SourceID) > maxSourceIDBytes:
		return wastedSpendRefusal(api.Coded(billing.CodeInvalidParam, "source and source_id (at most 255 bytes) required").WithParam("source_id"))
	case !serviceCustomerScopeAllows(r, params.CustomerID):
		return wastedSpendRefusal(api.Coded(billing.CodeServiceCredentialCustomerScopeDenied, ""))
	}
	report, err := svc.ReportWastedSpend(r.Request.Context(), billingservice.WastedSpendInput{
		CustomerID: params.CustomerID, Invoker: strings.TrimSpace(params.Invoker), InvokerType: params.InvokerType,
		Currency: params.Currency, Amount: params.Amount, Source: params.Source, SourceID: params.SourceID, Reason: params.Reason,
	})
	if err != nil {
		refusal := moneyRefusal(err)
		if refusal == nil {
			log.WithContext(r.Request.Context()).WithError(err).WithFields(log.Fields{"customer_id": params.CustomerID, "source_id": params.SourceID}).
				Error("wasted spend report failed")
			refusal = api.Coded(billing.CodeInternalError, "wasted spend report failed")
		}
		return wastedSpendRefusal(refusal)
	}
	return billing.WastedSpendResult{Status: http.StatusOK, Report: report}
}

// RecordUsage records a batch of usage events, one result per item in
// order. The batch answers 200; each item is recorded or refused on its own,
// exactly as recording it alone would be.
func RecordUsage(r *httprequest.Request) {
	var params billing.RecordUsageBatchParams
	if !r.BindJSON(&params) {
		return
	}
	if !batchItems(r, len(params.Items), billing.MaxUsageBatchItems) {
		return
	}
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out := make([]billing.UsageEventResult, len(params.Items))
	for i, item := range params.Items {
		out[i] = recordUsageItem(r, svc, item)
		if out[i].Error != nil {
			out[i].Error.RequestID = r.RequestID()
		}
	}
	r.JSON(http.StatusOK, billing.RecordUsageBatchResult{Items: out})
}

func usageRefusal(err *api.APIError) billing.UsageEventResult {
	details := err.ToResponse().Error
	return billing.UsageEventResult{Status: err.HTTPStatus, Error: &details}
}

func recordUsageItem(r *httprequest.Request, svc *billingservice.Service, params billing.RecordUsageParams) billing.UsageEventResult {
	switch {
	case params.Amount < 0:
		return usageRefusal(api.Coded(billing.CodeInvalidParam, "amount must be nonnegative").WithParam("amount"))
	case params.CustomerID.IsZero():
		return usageRefusal(api.Coded(billing.CodeInvalidParam, "customer_id required").WithParam("customer_id"))
	case len(params.SourceID) > maxSourceIDBytes:
		return usageRefusal(api.Coded(billing.CodeInvalidParam, "source_id must be at most 255 bytes").WithParam("source_id"))
	case !serviceCustomerScopeAllows(r, params.CustomerID):
		return usageRefusal(api.Coded(billing.CodeServiceCredentialCustomerScopeDenied, ""))
	}
	key, err := money.NewIdempotencyKey(money.UsageOperation(params.EventType), params.Source, params.SourceID)
	if err != nil {
		return usageRefusal(api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("source_id"))
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
		refusal := moneyRefusal(err)
		if refusal == nil {
			// The wire stays non-leaky; the cause reaches the operator log.
			log.WithContext(r.Request.Context()).WithError(err).WithFields(log.Fields{"customer_id": params.CustomerID, "source_id": params.SourceID}).
				Error("usage record failed")
			refusal = api.Coded(billing.CodeInternalError, "usage record failed")
		}
		return usageRefusal(refusal)
	}
	status := http.StatusCreated
	if event.Replayed {
		status = http.StatusOK
	}
	return billing.UsageEventResult{Status: status, Event: event}
}

// GetCustomerUsage reports a customer's usage.
func GetCustomerUsage(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	getUsage(r, customer)
}
