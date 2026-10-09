package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// ListCustomerSettings lists customers' settings, newest customer first, or
// the ones ids names.
func ListCustomerSettings(r *httprequest.Request) {
	var params billing.CustomerSettingsListParams
	var ok bool
	if params.IDs, ok = listIDs(r, billing.ParseCustomerID); !ok {
		return
	}
	if params.PageRequest, ok = r.Page(); !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	page, err := svc.ListCustomerSettings(r.Request.Context(), params)
	if err != nil {
		writeRefusal(r, err, "customer settings list failed")
		return
	}
	r.SuccessJSON(page)
}

// UpdateCustomerSettings changes customers' settings, all or none.
func UpdateCustomerSettings(r *httprequest.Request) {
	var req billing.UpdateCustomerSettingsBatchParams
	if !r.BindJSON(&req) {
		return
	}
	if !batchItems(r, len(req.Items), billing.MaxBatchItems) {
		return
	}
	for _, item := range req.Items {
		if !item.CustomerID.IsZero() && !requireServiceCustomerScope(r, item.CustomerID) {
			return
		}
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.UpdateCustomerSettings(r.Request.Context(), req.Items)
	if err != nil {
		writeRefusal(r, err, "customer settings update failed")
		return
	}
	r.SuccessJSON(billing.CustomerSettingsBatch{Items: out})
}
