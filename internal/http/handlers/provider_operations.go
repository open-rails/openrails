package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Provider-operation routes are the Client form of the provider obligation
// commands. Request bodies decode strictly: an unknown field such as a
// caller-rated settlement amount is refused, never ignored.

func ServiceOpenProviderOperation(r *httprequest.Request) {
	var req billing.OpenProviderOperationParams
	svc, ok := providerOperationService(r, &req)
	if !ok {
		return
	}
	out, err := svc.OpenProviderOperation(r.Request.Context(), req)
	if err == nil && !out.Replayed {
		r.JSON(http.StatusCreated, out)
		return
	}
	writeProviderOperation(r, out, err)
}

func ServiceGetProviderOperation(r *httprequest.Request) {
	svc, ok := providerOperationService(r, nil)
	if !ok {
		return
	}
	out, err := svc.GetProviderOperation(r.Request.Context(), r.Param("operation_id"))
	writeProviderOperation(r, out, err)
}

func ServiceIncrementProviderOperation(r *httprequest.Request) {
	var req billing.IncrementProviderOperationParams
	svc, ok := providerOperationService(r, &req)
	if !ok {
		return
	}
	req.OperationID = r.Param("operation_id")
	out, err := svc.IncrementProviderOperation(r.Request.Context(), req)
	writeProviderOperation(r, out, err)
}

func ServiceReleaseProviderOperation(r *httprequest.Request) {
	var req billing.ReleaseProviderOperationParams
	svc, ok := providerOperationService(r, &req)
	if !ok {
		return
	}
	req.OperationID = r.Param("operation_id")
	out, err := svc.ReleaseProviderOperation(r.Request.Context(), req)
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

func ServiceCloseProviderOperation(r *httprequest.Request) {
	var req billing.CloseProviderOperationParams
	svc, ok := providerOperationService(r, &req)
	if !ok {
		return
	}
	req.OperationID = r.Param("operation_id")
	out, err := svc.CloseProviderOperation(r.Request.Context(), req)
	writeProviderOperation(r, out, err)
}

// ServiceListProviderOperations lists operations newest first.
//
//	GET /admin/provider-operations?state&refused&limit&cursor
func ServiceListProviderOperations(r *httprequest.Request) {
	svc, ok := providerOperationService(r, nil)
	if !ok {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	q := queryReader{r: r}
	filter := billing.ProviderOperationListParams{PageRequest: page}
	for _, v := range q.list("state") {
		filter.State = append(filter.State, billing.ProviderOperationState(v))
	}
	if raw := strings.TrimSpace(r.Query("refused")); raw != "" {
		refused, err := strconv.ParseBool(raw)
		if err != nil {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "refused must be true or false").WithParam("refused"))
			return
		}
		filter.Refused = &refused
	}
	out, err := svc.ListProviderOperations(r.Request.Context(), filter)
	if err != nil {
		writeRefusal(r, err, "provider operations could not be listed")
		return
	}
	r.SuccessJSON(out)
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
	var operationConflict *billing.ProviderOperationConflict
	var observationConflict *billing.ProviderBillingObservationConflict
	switch {
	case errors.As(err, &operationConflict):
		out.Param = &operationConflict.Field
	case errors.As(err, &observationConflict):
		out.Param = &observationConflict.Field
	}
	r.APIError(out)
}
