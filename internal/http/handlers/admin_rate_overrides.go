package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// or#909 merchant-admin negotiated price overrides: per-customer rate cards
// that replace the merchant-default card for a catalog meter, with an
// optional included allowance netted before overage at rating time.
// Admin surface — negotiated pricing is never self-serve.

// adminRateOverrideRequest is the PUT body. Price is the canonical
// catalog.RatePrice charge-model JSON (same shape the catalog speaks);
// allowance.included is the pre-overage quantity in the meter's raw unit.
type AdminRateOverrideRequest struct {
	Price     catalog.RatePrice  `json:"price" binding:"required"`
	Allowance *catalog.Allowance `json:"allowance"`
}

// PutAdminRateOverride is PUT /v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}:
// install (or replace) the payer's negotiated card for one meter. Idempotent.
func PutAdminRateOverride(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	meterKey := catalogrules.NormalizeKey(r.Param("meter_key"))
	if meterKey == "" {
		r.ErrorJSON(http.StatusBadRequest, "meter_key required")
		return
	}
	var req AdminRateOverrideRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetUsageRateCard(r.Request.Context(), billingservice.UsageRateCardInput{
		Payer:     payer,
		MeterKey:  meterKey,
		Price:     req.Price,
		Allowance: req.Allowance,
	}); err != nil {
		writeMeteringError(r, err)
		return
	}
	cards, err := svc.ListPayerRateCards(r.Request.Context(), *payer)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "override stored but read-back failed")
		return
	}
	for _, c := range cards {
		if c.MeterKey == meterKey {
			r.SuccessJSON(c)
			return
		}
	}
	r.ErrorJSON(http.StatusInternalServerError, "override stored but read-back failed")
}

// ListAdminRateOverrides is GET /v1/merchant/customers/{customer_id}/rate-overrides.
func ListAdminRateOverrides(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	cards, err := svc.ListPayerRateCards(r.Request.Context(), *payer)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list rate overrides")
		return
	}
	r.SuccessJSON(cards)
}

// DeleteAdminRateOverride is DELETE /v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}:
// drop the negotiated card, restoring the merchant default for future rating.
func DeleteAdminRateOverride(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	meterKey := catalogrules.NormalizeKey(r.Param("meter_key"))
	if meterKey == "" {
		r.ErrorJSON(http.StatusBadRequest, "meter_key required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.DeletePayerRateCard(r.Request.Context(), *payer, meterKey); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to delete rate override")
		return
	}
	r.SuccessJSONMessage("rate override removed (merchant default restored)")
}
