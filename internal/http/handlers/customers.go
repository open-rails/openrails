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

// EnsureCustomer creates a customer or replaces its declared fields.
func EnsureCustomer(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.CustomerParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.EnsureCustomer(r.Request.Context(), customer, params)
	if err != nil {
		writeRefusal(r, err, "customer update failed")
		return
	}
	r.SuccessJSON(out)
}

// GetCustomerBillingPolicy reads the policy assigned to a customer.
func GetCustomerBillingPolicy(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.GetCustomerBillingPolicy(r.Request.Context(), customer)
	if err != nil {
		writeRefusal(r, err, "customer policy read failed")
		return
	}
	r.SuccessJSON(out)
}

// SetCustomerBillingPolicy assigns a declared policy to a customer, or
// clears the assignment.
func SetCustomerBillingPolicy(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.CustomerBillingPolicyParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.SetCustomerBillingPolicy(r.Request.Context(), customer, params.PolicyName)
	if err != nil {
		writeRefusal(r, err, "customer policy assignment failed")
		return
	}
	r.SuccessJSON(out)
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
