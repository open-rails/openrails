package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/open-rails/openrails/billing"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// ServiceGetMerchantSettings returns the complete declarative policy document.
func ServiceGetMerchantSettings(r *httprequest.Request) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	settings, err := svc.GetMerchantSettings(r.Request.Context())
	if err != nil {
		r.InternalError("get merchant settings failed", err)
		return
	}
	r.SuccessJSON(settings)
}

// ServiceSetMerchantSettings atomically replaces the declarative policy document.
func ServiceSetMerchantSettings(r *httprequest.Request) {
	var settings *billing.MerchantSettings
	decoder := json.NewDecoder(r.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			r.ErrorJSON(http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var extra any
	if settings == nil || decoder.Decode(&extra) != io.EOF {
		r.ErrorJSON(http.StatusBadRequest, "one settings document required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	if err := svc.SetMerchantSettings(r.Request.Context(), *settings); err != nil {
		if errors.Is(err, billingservice.ErrInvalidMerchantSettings) {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		r.InternalError("set merchant settings failed", err)
		return
	}
	r.SuccessJSONMessage("ok")
}
