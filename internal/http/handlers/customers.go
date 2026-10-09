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

// GetCustomers reads up to billing.MaxCustomerLookup customers; one that
// does not exist is null. A credential scoped to some customers may name only
// those.
func GetCustomers(r *httprequest.Request) {
	var req billing.CustomerLookupParams
	if !r.BindJSON(&req) {
		return
	}
	ids, ok := batchIDs(r, req.CustomerIDs, billing.MaxCustomerLookup, "customer_ids")
	if !ok {
		return
	}
	for _, id := range ids {
		if !requireServiceCustomerScope(r, id) {
			return
		}
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.GetCustomers(r.Request.Context(), ids)
	if err != nil {
		writeRefusal(r, err, "customer read failed")
		return
	}
	r.SuccessJSON(billing.CustomerLookup{Customers: out})
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
	var params billing.SetCustomerBillingPolicyParams
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
