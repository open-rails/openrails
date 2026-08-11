package handlers

import (
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/pricing"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#909 merchant-admin negotiated price overrides: per-customer rate cards
// that replace the merchant-default card for a catalog meter, with an
// optional included allowance netted before overage at rating time.
// Admin surface — negotiated pricing is never self-serve.

// adminRateOverrideRequest is the PUT body. Price is the canonical
// pricing.RatePrice charge-model JSON (same shape the catalog speaks);
// allowance.included is the pre-overage quantity in the meter's raw unit.
type adminRateOverrideRequest struct {
	Price     pricing.RatePrice  `json:"price" binding:"required"`
	Allowance *pricing.Allowance `json:"allowance"`
}

// PutAdminRateOverride is PUT /v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}:
// install (or replace) the payer's negotiated card for one meter. Idempotent.
func PutAdminRateOverride(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	meterKey := strings.TrimSpace(r.Param("meter_key"))
	if meterKey == "" {
		r.ErrorJSON(http.StatusBadRequest, "meter_key required")
		return
	}
	var req adminRateOverrideRequest
	if !r.BindJSON(&req) {
		return
	}
	if strings.TrimSpace(req.Price.Model) == "" {
		r.ErrorJSON(http.StatusBadRequest, "price.model required")
		return
	}
	if req.Allowance != nil && req.Allowance.Included < 0 {
		r.ErrorJSON(http.StatusBadRequest, "allowance.included must be >= 0")
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
		r.ErrorJSON(http.StatusBadRequest, err.Error())
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
	meterKey := strings.TrimSpace(r.Param("meter_key"))
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
