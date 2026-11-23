package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	billingidentity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	billingservice "github.com/open-rails/openrails/internal/service"
)

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
	case billing.AdmissionBlockedByAbuse, billing.AdmissionBlockedByLimit:
		return http.StatusTooManyRequests
	case billing.AdmissionBlockedByMoney:
		return http.StatusPaymentRequired
	}
	return http.StatusForbidden
}

func admitRefusal(code billing.ErrorCode, message, param string) billing.AdmissionVerdict {
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
	ctx context.Context, logger *slog.Logger,
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
			if code, ok := billing.ErrorCodeFrom(err); ok {
				out[i] = admitRefusal(code, err.Error(), "")
				continue
			}
			// The wire stays non-leaky; the cause reaches the operator log.
			logger.ErrorContext(ctx, "admission check failed", logx.Err(err), "customer_id", item.CustomerID, "invoker", item.Invoker, "request_id", item.RequestID)
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
	if err := params.Validate(); err != nil {
		writeRefusal(r, err, "admit batch refused")
		return
	}
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	verdicts := AdmitVerdicts(r.Request.Context(), r.Log, params.Items,
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
	if err != nil {
		return admissionLookupRefusal(r, requestID, err)
	}
	if !serviceCustomerScopeAllows(r, billingidentity.CustomerID(customer)) {
		return api.Coded(billing.CodeServiceCredentialCustomerScopeDenied, "")
	}
	return nil
}

// admissionLookupRefusal answers a failed lookup: a typed refusal keeps its
// code (database_busy is a retry), anything else is a 500.
func admissionLookupRefusal(r *httprequest.Request, requestID string, err error) *api.APIError {
	if errors.Is(err, spendgate.ErrNotFound) {
		return api.Coded(billing.CodeAdmissionNotFound, "")
	}
	if out := refusalOf(err); out != nil {
		return out
	}
	r.Log.ErrorContext(r.Request.Context(), "admission lookup failed", logx.Err(err), "request_id", requestID)
	return api.Coded(billing.CodeInternalError, "admission lookup failed")
}

func admissionRefusal(err *api.APIError) billing.AdmissionResult {
	details := err.ToResponse().Error
	return billing.AdmissionResult{Status: err.HTTPStatus, Error: &details}
}

// admissionBatch answers one result per item of a valid batch, in order:
// each runs on its own, exactly as it would alone.
func admissionBatch(r *httprequest.Request, valid error, n int, run func(*billingservice.Service, int) billing.AdmissionResult) {
	if valid != nil {
		writeRefusal(r, valid, "admission batch refused")
		return
	}
	if !requireMerchantRoutePrincipal(r) {
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
	if err := params.Validate(); err != nil {
		writeRefusal(r, err, "capture batch refused")
		return
	}
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out := CaptureResults(r.Request.Context(), r.Log, params.Items, func(requestID string) *api.APIError {
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
	ctx context.Context, logger *slog.Logger,
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
			out[i] = refused(api.Coded(billing.CodeAdmissionNotFound, ""))
		default:
			out[i] = refused(admissionFailure(ctx, logger, err, item.RequestID, "capture failed"))
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
	admissionBatch(r, params.Validate(), len(params.RequestIDs), func(svc *billingservice.Service, i int) billing.AdmissionResult {
		requestID := strings.TrimSpace(params.RequestIDs[i])
		if refusal := admissionItemScope(r, svc, requestID); refusal != nil {
			return admissionRefusal(refusal)
		}
		return ReleaseResult(r.Request.Context(), r.Log, requestID, svc.ReleaseAdmission)
	})
}

// ReleaseResult releases one admission.
func ReleaseResult(ctx context.Context, logger *slog.Logger, requestID string, release func(context.Context, string) (*billing.Admission, error)) billing.AdmissionResult {
	if requestID == "" {
		return admissionRefusal(api.Coded(billing.CodeInvalidParam, "request_id required").WithParam("request_id"))
	}
	admission, err := release(ctx, requestID)
	switch {
	case err == nil:
		return billing.AdmissionResult{Status: http.StatusOK, Admission: admission}
	case errors.Is(err, spendgate.ErrNotFound):
		return admissionRefusal(api.Coded(billing.CodeAdmissionNotFound, ""))
	case errors.Is(err, spendgate.ErrCaptured):
		return admissionRefusal(api.Coded(billing.CodeAdmissionCaptured, ""))
	}
	return admissionRefusal(admissionFailure(ctx, logger, err, requestID, "release failed"))
}

// ExtendAdmissions moves open holds' deadlines later, one result per item.
func ExtendAdmissions(r *httprequest.Request) {
	var params billing.ExtendAdmissionBatchParams
	if !r.BindJSON(&params) {
		return
	}
	admissionBatch(r, params.Validate(), len(params.Items), func(svc *billingservice.Service, i int) billing.AdmissionResult {
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
			return admissionRefusal(api.Coded(billing.CodeHoldNotFound, ""))
		case errors.Is(err, billingservice.ErrHoldDeadlinePassed):
			return admissionRefusal(api.Coded(billing.CodeInvalidParam, "expires_at already passed").WithParam("expires_at"))
		case errors.Is(err, spendgate.ErrDeadlineShortened):
			return admissionRefusal(api.Coded(billing.CodeInvalidParam, "an extension cannot shorten the deadline").WithParam("expires_at"))
		case errors.As(err, &invalid):
			return admissionRefusal(api.Coded(billing.CodeInvalidParam, invalid.Message).WithParam(invalid.Param))
		}
		return admissionRefusal(admissionFailure(r.Request.Context(), r.Log, err, requestID, "extend failed"))
	})
}

// admissionFailure is a money refusal, or an internal error whose cause
// reaches the operator log and never the wire.
func admissionFailure(ctx context.Context, logger *slog.Logger, err error, requestID, message string) *api.APIError {
	if refusal := moneyRefusal(err); refusal != nil {
		return refusal
	}
	logger.ErrorContext(ctx, message, logx.Err(err), "request_id", requestID)
	return api.Coded(billing.CodeInternalError, message)
}
