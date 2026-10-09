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

// GetCustomer reads one customer.
func GetCustomer(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.GetCustomer(r.Request.Context(), customer)
	if err != nil {
		writeRefusal(r, err, "customer read failed")
		return
	}
	r.SuccessJSON(out)
}
