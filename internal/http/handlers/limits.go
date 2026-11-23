package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// ListLimits is one page of the merchant's limits, by key.
func ListLimits(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListLimits(r.Request.Context(), page)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// GetLimit reads the limit under the path's key.
func GetLimit(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetLimit(r.Request.Context(), r.Param("key"))
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// SetLimit declares the limit under the path's key whole.
func SetLimit(r *httprequest.Request) {
	var params billing.SetLimitParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.SetLimit(r.Request.Context(), r.Param("key"), params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// ListCustomerLimits is where ?customer_id= stands on every limit.
func ListCustomerLimits(r *httprequest.Request) {
	id, err := billing.ParseCustomerID(strings.TrimSpace(r.Query("customer_id")))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded("invalid_customer_id", "").WithParam("customer_id"))
		return
	}
	if !requireServiceCustomerScope(r, id) {
		return
	}
	listCustomerLimits(r, identity.CustomerID(id))
}

// ListMyLimits is where the signed-in customer stands on every limit.
func ListMyLimits(r *httprequest.Request) {
	customer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	listCustomerLimits(r, customer)
}

func listCustomerLimits(r *httprequest.Request, customer identity.CustomerID) {
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.ListCustomerLimits(r.Request.Context(), customer)
	if err != nil {
		writeRefusal(r, err, "limit read failed")
		return
	}
	r.SuccessJSON(out)
}

// AdjustCustomerLimit adds to the capacity of the customer's current window
// of one limit.
func AdjustCustomerLimit(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.AdjustLimitParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.AdjustCustomerLimit(r.Request.Context(), identity.CustomerID(customer), params)
	if err != nil {
		writeRefusal(r, err, "limit adjustment failed")
		return
	}
	r.SuccessJSON(out)
}

// SyncLeases is a gate's lease sync. The route is declared for gates; this
// build answers not_implemented.
func SyncLeases(r *httprequest.Request) {
	var params billing.SyncLeasesParams
	if !r.BindJSON(&params) {
		return
	}
	r.ErrorCode(billing.CodeNotImplemented, "lease syncs are not served by this build")
}
