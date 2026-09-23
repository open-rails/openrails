package handlers

import (
	"github.com/open-rails/openrails"
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

func ApplyMerchantConfiguration(r *httprequest.Request) {
	var params openrails.MerchantConfigurationApplyParams
	if !r.BindJSON(&params) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	receipt, err := svc.ApplyMerchantConfiguration(r.Request.Context(), params)
	if err != nil {
		writeRefusal(r, err, "apply merchant configuration failed")
		return
	}
	r.SuccessJSON(receipt)
}
