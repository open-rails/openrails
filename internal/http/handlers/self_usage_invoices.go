package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// GetMyUsage reports the customer's own usage.
func GetMyUsage(r *httprequest.Request) {
	customer, ok := selfAccountCustomer(r)
	if !ok {
		return
	}
	getUsage(r, customer)
}

// GetUsage reports a customer's usage to staff.
func GetUsage(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	getUsage(r, customer)
}

// GetMyUsageSeries reports the customer's own usage over time.
func GetMyUsageSeries(r *httprequest.Request) {
	customer, ok := selfAccountCustomer(r)
	if !ok {
		return
	}
	getUsageSeries(r, customer)
}

// GetUsageSeries reports a customer's usage over time to staff.
func GetUsageSeries(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	getUsageSeries(r, customer)
}

// getUsageSeries answers a customer's usage series.
func getUsageSeries(r *httprequest.Request, customer billing.CustomerID) {
	var params billing.GetUsageSeriesParams
	if !r.BindQuery(&params) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	series, err := svc.GetUsageSeries(r.Request.Context(), customer, params)
	if err != nil {
		writeMoneyError(r, err, "usage read failed")
		return
	}
	r.SuccessJSON(series)
}

// getUsage answers a page of a customer's usage report. ?from= and ?to= are
// RFC 3339 or a date; left out, the cursor's window or the month before now.
func getUsage(r *httprequest.Request, customer billing.CustomerID) {
	var params billing.GetUsageParams
	if !r.BindQuery(&params) {
		return
	}
	var ok bool
	if params.PageRequest, ok = r.PageOf(billingservice.UsagePage); !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	usage, err := svc.GetUsage(r.Request.Context(), customer, params)
	if err != nil {
		writeMoneyError(r, err, "usage read failed")
		return
	}
	r.SuccessJSON(usage)
}
