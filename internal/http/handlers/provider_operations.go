package handlers

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Provider-operation routes are the Client form of the provider obligation
// commands. Request bodies decode strictly: an unknown field such as a
// caller-rated settlement amount is refused, never ignored.

func ServiceOpenOperationAuthorization(r *httprequest.Request) {
	var req billing.OpenOperationAuthorizationParams
	svc, ok := providerOperationService(r, &req)
	if !ok {
		return
	}
	out, err := svc.OpenOperationAuthorization(r.Request.Context(), req)
	writeProviderOperation(r, out, err)
}

func ServiceGetOperationAuthorization(r *httprequest.Request) {
	svc, ok := providerOperationService(r, nil)
	if !ok {
		return
	}
	out, err := svc.GetOperationAuthorization(r.Request.Context(), r.Param("operation_id"))
	writeProviderOperation(r, out, err)
}

func ServiceReleaseOperationAuthorization(r *httprequest.Request) {
	var req billing.ReleaseOperationAuthorizationParams
	svc, ok := providerOperationService(r, &req)
	if !ok {
		return
	}
	req.OperationID = r.Param("operation_id")
	out, err := svc.ReleaseOperationAuthorization(r.Request.Context(), req)
	writeProviderOperation(r, out, err)
}

func ServiceRecordProviderBillingObservation(r *httprequest.Request) {
	var req billing.RecordProviderBillingObservationParams
	svc, ok := providerOperationService(r, &req)
	if !ok {
		return
	}
	req.OperationID = r.Param("operation_id")
	out, err := svc.RecordProviderBillingObservation(r.Request.Context(), req)
	writeProviderOperation(r, out, err)
}

func ServiceGetProviderBillingQualification(r *httprequest.Request) {
	svc, ok := providerOperationService(r, nil)
	if !ok {
		return
	}
	out, err := svc.GetProviderBillingQualification(r.Request.Context(), r.Param("operation_id"))
	writeProviderOperation(r, out, err)
}

// providerOperationService authenticates the merchant principal and, for a
// write, strictly decodes exactly one JSON object into body.
func providerOperationService(r *httprequest.Request, body any) (*billingservice.Service, bool) {
	if !requireMerchantRoutePrincipal(r) {
		return nil, false
	}
	if body != nil && !r.BindJSON(body) {
		return nil, false
	}
	return newAdminBillingService(r)
}

func writeProviderOperation(r *httprequest.Request, out any, err error) {
	if err != nil {
		writeProviderOperationError(r, err)
		return
	}
	r.SuccessJSON(out)
}

// writeProviderOperationError reports the same classification the host
// transaction extension returns: a coded sentinel keeps its code and class.
func writeProviderOperationError(r *httprequest.Request, err error) {
	var coded interface{ ErrorCode() string }
	var out *api.APIError
	switch {
	case errors.Is(err, billing.ErrInsufficientCredits):
		out = api.NewAPIError(http.StatusPaymentRequired, api.ErrorTypeCard, api.CodeInsufficientCredits, "Insufficient credits")
	case errors.As(err, &coded) && errors.Is(err, billing.ErrNotFound):
		out = api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, coded.ErrorCode(), err.Error())
	case errors.As(err, &coded) && errors.Is(err, billing.ErrConflict):
		out = api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, coded.ErrorCode(), err.Error())
	case errors.Is(err, billing.ErrInvalid):
		out = api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, api.CodeInvalidParam, err.Error())
	default:
		r.InternalError("provider operation failed", err)
		return
	}
	var authorizationConflict *billing.OperationAuthorizationConflict
	var observationConflict *billing.ProviderBillingObservationConflict
	switch {
	case errors.As(err, &authorizationConflict):
		out.Param = &authorizationConflict.Field
	case errors.As(err, &observationConflict):
		out.Param = &observationConflict.Field
	}
	r.APIError(out)
}
