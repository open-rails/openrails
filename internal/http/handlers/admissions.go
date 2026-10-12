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
		Usage:                   p.Usage,
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

// AdmitVerdicts admits each item on its own: one item's bad input, scope
// denial, refusal or backend error never fails another.
func AdmitVerdicts(
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
		case refusalOf(err) != nil:
			details := refusalOf(err).ToResponse().Error
			out[i] = billing.AdmissionVerdict{Status: refusalOf(err).HTTPStatus, Error: &details}
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
	verdicts := AdmitVerdicts(r.Request.Context(), params.Items,
		func(c billing.CustomerID) bool { return serviceCustomerScopeAllows(r, c) }, svc.Admit)
	for i := range verdicts {
		if verdicts[i].Error != nil {
			verdicts[i].Error.RequestID = r.RequestID()
		}
	}
	r.JSON(http.StatusOK, billing.AdmitBatchResult{Items: verdicts})
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

// CaptureAdmissions settles admitted requests, one result per item in order:
// each is captured or refused on its own, exactly as capturing it alone.
func CaptureAdmissions(r *httprequest.Request) {
	var params billing.CaptureAdmissionBatchParams
	if !r.BindJSON(&params) {
		return
	}
	if !batchItems(r, len(params.Items), billing.MaxAdmissionBatchItems) || !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out := CaptureResults(r.Request.Context(), params.Items, func(requestID string) *api.APIError {
		return admissionItemScope(r, svc, requestID)
	}, svc.CaptureAdmission)
	for i := range out {
		if out[i].Error != nil {
			out[i].Error.RequestID = r.RequestID()
		}
	}
	r.JSON(http.StatusOK, billing.CaptureBatchResult{Items: out})
}

// CaptureResults captures each item on its own. scope, when set, refuses an
// item the credential may not settle.
func CaptureResults(
	ctx context.Context,
	items []billing.CaptureAdmissionParams,
	scope func(requestID string) *api.APIError,
	capture func(context.Context, billing.CaptureAdmissionParams) (*billing.CaptureReceipt, error),
) []billing.CaptureResult {
	refused := func(err *api.APIError) billing.CaptureResult {
		details := err.ToResponse().Error
		return billing.CaptureResult{Status: err.HTTPStatus, Error: &details}
	}
	out := make([]billing.CaptureResult, len(items))
	for i, item := range items {
		item.RequestID = strings.TrimSpace(item.RequestID)
		if item.RequestID == "" {
			out[i] = refused(api.Coded(billing.CodeInvalidParam, "request_id required").WithParam("request_id"))
			continue
		}
		if scope != nil {
			if refusal := scope(item.RequestID); refusal != nil {
				out[i] = refused(refusal)
				continue
			}
		}
		receipt, err := capture(ctx, item)
		switch {
		case err == nil:
			out[i] = billing.CaptureResult{Status: http.StatusOK, Receipt: receipt}
		case errors.Is(err, spendgate.ErrNotFound):
			out[i] = refused(api.Coded("admission_not_found", ""))
		default:
			out[i] = refused(admissionFailure(ctx, err, item.RequestID, "capture failed"))
		}
	}
	return out
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
		return ReleaseResult(r.Request.Context(), requestID, svc.ReleaseAdmission)
	})
}

// ReleaseResult releases one admission.
func ReleaseResult(ctx context.Context, requestID string, release func(context.Context, string) (*billing.Admission, error)) billing.AdmissionResult {
	if requestID == "" {
		return admissionRefusal(api.Coded(billing.CodeInvalidParam, "request_id required").WithParam("request_id"))
	}
	admission, err := release(ctx, requestID)
	switch {
	case err == nil:
		return billing.AdmissionResult{Status: http.StatusOK, Admission: admission}
	case errors.Is(err, spendgate.ErrNotFound):
		return admissionRefusal(api.Coded("admission_not_found", ""))
	case errors.Is(err, spendgate.ErrCaptured):
		return admissionRefusal(api.Coded("admission_captured", ""))
	}
	return admissionRefusal(admissionFailure(ctx, err, requestID, "release failed"))
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
		return admissionRefusal(admissionFailure(r.Request.Context(), err, requestID, "extend failed"))
	})
}

// admissionFailure is a money refusal, or an internal error whose cause
// reaches the operator log and never the wire.
func admissionFailure(ctx context.Context, err error, requestID, message string) *api.APIError {
	if refusal := moneyRefusal(err); refusal != nil {
		return refusal
	}
	log.WithContext(ctx).WithError(err).WithField("request_id", requestID).Error(message)
	return api.Coded(billing.CodeInternalError, message)
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
	case params.Outcome != "" && params.Outcome != billing.UsageSucceeded && params.Outcome != billing.UsageFailed:
		return usageRefusal(api.Coded(billing.CodeInvalidParam, `outcome must be "succeeded" or "failed"`).WithParam("outcome"))
	case params.InvokerType != "" && params.InvokerType != billing.InvokerTypeCustomer && params.InvokerType != billing.InvokerTypeDelegated:
		return usageRefusal(api.Coded(billing.CodeInvalidParam, `invoker_type must be "customer" or "delegated"`).WithParam("invoker_type"))
	case params.InvokerType == billing.InvokerTypeDelegated && (params.Outcome != billing.UsageFailed || strings.TrimSpace(params.Invoker) == ""):
		return usageRefusal(api.Coded(billing.CodeInvalidParam, "a delegated invoker records only failed usage, and names its invoker").WithParam("invoker_type"))
	case !serviceCustomerScopeAllows(r, params.CustomerID):
		return usageRefusal(api.Coded(billing.CodeServiceCredentialCustomerScopeDenied, ""))
	}
	key, err := money.NewIdempotencyKey(money.UsageOperation(params.EventType), params.Source, params.SourceID)
	if err != nil {
		return usageRefusal(api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("source_id"))
	}
	in := billingservice.RecordUsageInput{
		CustomerID: params.CustomerID, Invoker: params.Invoker, InvokerType: params.InvokerType, Currency: params.Currency, EventType: params.EventType,
		Dimensions: params.Dimensions, Amount: params.Amount, Resource: params.Resource, Metadata: params.Metadata, Key: key,
		Failed: params.Outcome == billing.UsageFailed,
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
