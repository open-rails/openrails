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
		r.ErrorCode(billing.CodeAdmissionNotFound, "")
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
