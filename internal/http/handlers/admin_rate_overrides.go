package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Rate overrides are a customer's negotiated price for one meter, replacing
// the meter's rate card when that customer's usage is rated. Merchant staff
// set them; customers never do.

// SetRateOverride sets the customer's price for the path's meter.
func SetRateOverride(r *httprequest.Request) {
	customer, ok := rateOverrideCustomer(r)
	if !ok {
		return
	}
	key, ok := meterKeyParam(r, "meter_key")
	if !ok {
		return
	}
	var params billing.SetRateOverrideParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	if err := svc.SetUsageRateCard(r.Request.Context(), billingservice.UsageRateCardInput{Payer: &customer, MeterKey: key, Price: params.Price, Allowance: params.Allowance, ExpectedRevision: params.ExpectedRevision}); err != nil {
		writeMeteringError(r, err)
		return
	}
	out, err := svc.GetPayerRateCard(r.Request.Context(), customer, key)
	if err != nil {
		writeMeteringError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// ListRateOverrides lists customers' negotiated prices, by customer then
// meter: of ?customer_id= and ?meter_key= when given.
func ListRateOverrides(r *httprequest.Request) {
	customer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil {
		r.APIError(api.Coded(billing.CodeInvalidQuery, "invalid customer_id").WithParam("customer_id"))
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListRateOverrides(r.Request.Context(), customer, r.Query("meter_key"), page)
	if err != nil {
		writeMeteringError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// DeleteRateOverride removes a customer's negotiated price; the meter's rate
// card prices their usage again.
func DeleteRateOverride(r *httprequest.Request) {
	customer, ok := rateOverrideCustomer(r)
	if !ok {
		return
	}
	key, ok := meterKeyParam(r, "meter_key")
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	if err := svc.DeletePayerRateCard(r.Request.Context(), customer, key); err != nil {
		writeMeteringError(r, err)
		return
	}
	r.Status(http.StatusNoContent)
}

func rateOverrideCustomer(r *httprequest.Request) (identity.CustomerID, bool) {
	customer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || customer == nil {
		r.APIError(invalidParam("customer_id", "invalid customer_id"))
		return identity.CustomerID{}, false
	}
	return *customer, true
}
