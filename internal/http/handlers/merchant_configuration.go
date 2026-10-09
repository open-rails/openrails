package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

func GetMerchantConfiguration(r *httprequest.Request) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	state, err := svc.GetMerchantConfigurationState(r.Request.Context())
	if err != nil {
		writeRefusal(r, err, "read merchant configuration failed")
		return
	}
	r.SuccessJSON(state)
}

// UpdateMerchantConfiguration merges the body into the merchant's
// configuration at the revision it names, once per Idempotency-Key.
func UpdateMerchantConfiguration(r *httprequest.Request) {
	var params billing.UpdateMerchantConfigurationParams
	if !r.BindJSON(&params) {
		return
	}
	params.IdempotencyKey = r.Header("Idempotency-Key")
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	receipt, err := svc.UpdateMerchantConfiguration(r.Request.Context(), params)
	if err != nil {
		writeRefusal(r, err, "update merchant configuration failed")
		return
	}
	r.SuccessJSON(receipt)
}
