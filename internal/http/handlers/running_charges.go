package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// The running-charge routes are the Client form of the commands an embedding
// host can also run inside its own transaction. A charge is named by the
// host's reference.

// StartRunningCharge starts a charge: 201, or 200 for one already started
// under the reference with the same terms.
func StartRunningCharge(r *httprequest.Request) {
	var params billing.StartRunningChargeParams
	if !r.BindJSON(&params) {
		return
	}
	if !requireServiceCustomerScope(r, params.CustomerID) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	charge, started, err := svc.StartRunningCharge(r.Request.Context(), params)
	if err != nil {
		writeMoneyError(r, err, "running charge start failed")
		return
	}
	if started {
		r.JSON(http.StatusCreated, charge)
		return
	}
	r.SuccessJSON(charge)
}

// StopRunningCharge stops a charge and answers it with its final amount.
func StopRunningCharge(r *httprequest.Request) {
	var params billing.StopRunningChargeParams
	if !r.BindOptionalJSON(&params) {
		return
	}
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	charge, err := svc.StopRunningCharge(r.Request.Context(), r.Param("reference"), params)
	if err != nil {
		writeMoneyError(r, err, "running charge stop failed")
		return
	}
	r.SuccessJSON(charge)
}

// GetRunningCharge reads one charge.
func GetRunningCharge(r *httprequest.Request) {
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	charge, err := svc.GetRunningCharge(r.Request.Context(), r.Param("reference"))
	if err != nil {
		writeMoneyError(r, err, "running charge read failed")
		return
	}
	r.SuccessJSON(charge)
}

// ListRunningCharges pages the merchant's charges, newest first.
//
//	GET /app/running-charges?customer_id&state&limit&cursor
func ListRunningCharges(r *httprequest.Request) {
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	var params billing.RunningChargeListParams
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
	page, err := svc.ListRunningCharges(r.Request.Context(), params)
	if err != nil {
		writeMoneyError(r, err, "running charge list failed")
		return
	}
	r.SuccessJSON(page)
}
