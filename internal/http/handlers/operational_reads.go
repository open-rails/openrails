package handlers

import (
	"errors"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
)

// GetAdmission inspects the current state without replaying an admission.
func GetAdmission(r *httprequest.Request) {
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.GetAdmission(r.Request.Context(), r.Param("request_id"))
	if errors.Is(err, spendgate.ErrNotFound) {
		r.ErrorCode("admission_not_found", "")
		return
	}
	if err != nil {
		writeMoneyError(r, err, "admission read failed")
		return
	}
	r.SuccessJSON(out)
}

// ListAdmissions reads one page of the merchant's durable admitted requests.
func ListAdmissions(r *httprequest.Request) {
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	var params billing.AdmissionListParams
	if !r.BindQuery(&params) {
		return
	}
	var ok bool
	if params.PageRequest, ok = r.Page(); !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.ListAdmissions(r.Request.Context(), params)
	if err != nil {
		writeMoneyError(r, err, "admission list failed")
		return
	}
	r.SuccessJSON(out)
}

// GetUsageEvent reads one recorded event by its public ID.
func GetUsageEvent(r *httprequest.Request) {
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	id, err := billing.ParseUsageEventID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid usage event id")
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.GetUsageEvent(r.Request.Context(), id)
	if err != nil {
		writeMoneyError(r, err, "usage event read failed")
		return
	}
	r.SuccessJSON(out)
}

// ListUsageEvents reads one bounded page of individual recorded events.
func ListUsageEvents(r *httprequest.Request) {
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	var params billing.UsageEventListParams
	if !r.BindQuery(&params) {
		return
	}
	var ok bool
	if params.PageRequest, ok = r.Page(); !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.ListUsageEvents(r.Request.Context(), params)
	if err != nil {
		writeMoneyError(r, err, "usage event list failed")
		return
	}
	r.SuccessJSON(out)
}
