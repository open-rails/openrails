package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// UpdateCustomer changes one customer's settings and answers the customer.
func UpdateCustomer(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.UpdateCustomerParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.UpdateCustomer(r.Request.Context(), customer, params)
	if err != nil {
		writeRefusal(r, err, "customer update failed")
		return
	}
	r.SuccessJSON(out)
}
