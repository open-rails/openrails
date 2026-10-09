package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// ListCustomers lists the merchant's customers, newest first.
func ListCustomers(r *httprequest.Request) {
	var params billing.CustomerListParams
	if !r.BindQuery(&params) {
		return
	}
	var ok bool
	if params.PageRequest, ok = r.Page(); !ok {
		return
	}
	if params.IDs, ok = listIDs(r, billing.ParseCustomerID); !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	page, err := svc.ListCustomers(r.Request.Context(), params)
	if err != nil {
		writeRefusal(r, err, "customer list failed")
		return
	}
	r.SuccessJSON(page)
}

// EnsureCustomers creates customers or replaces their declared fields, all
// or none. A credential scoped to some customers may name only those.
func EnsureCustomers(r *httprequest.Request) {
	var req billing.EnsureCustomerBatchParams
	if !r.BindJSON(&req) {
		return
	}
	if !batchItems(r, len(req.Items), billing.MaxBatchItems) {
		return
	}
	for _, item := range req.Items {
		if !item.ID.IsZero() && !requireServiceCustomerScope(r, item.ID) {
			return
		}
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.EnsureCustomers(r.Request.Context(), req.Items)
	if err != nil {
		writeRefusal(r, err, "customer update failed")
		return
	}
	r.SuccessJSON(billing.EnsureCustomerBatchResult{Items: out})
}

// ListCustomerDelinquency lists a customer's delinquency in every currency it
// has owed in.
func ListCustomerDelinquency(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	page, err := svc.ListCustomerDelinquency(r.Request.Context(), customer)
	if err != nil {
		writeRefusal(r, err, "delinquency read failed")
		return
	}
	r.SuccessJSON(page)
}

// ListDelinquency lists the merchant's overdue customers, oldest debt first.
func ListDelinquency(r *httprequest.Request) {
	var params billing.DelinquencyListParams
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
	page, err := svc.ListDelinquency(r.Request.Context(), params)
	if err != nil {
		writeRefusal(r, err, "delinquency list failed")
		return
	}
	r.SuccessJSON(page)
}
