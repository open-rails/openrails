package handlers

import (
	"net/http"
	"strings"
	"time"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/billinganalysis"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantBillingAnalysis handles GET /v1/merchant/billing-analysis. It reads
// retained provider evidence and returns daily settled signup/rebill counts,
// failed attempts, and the cumulative open-unbilled roster. Provider names are
// only a filter; the report itself is provider-neutral.
func MerchantBillingAnalysis(r *httprequest.Request) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "billing analysis unavailable")
		return
	}
	mid, err := merchant.Require(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "merchant scope is required")
		return
	}
	locName := strings.TrimSpace(r.Query("timezone"))
	if locName == "" {
		locName = "UTC"
	}
	loc, err := time.LoadLocation(locName)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid timezone")
		return
	}
	now := time.Now().UTC()
	from, err := parseBillingAnalysisTime(r.Query("from"), loc, now.AddDate(0, 0, -30))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid from: use RFC3339 or YYYY-MM-DD")
		return
	}
	to, err := parseBillingAnalysisTime(r.Query("to"), loc, now)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid to: use RFC3339 or YYYY-MM-DD")
		return
	}
	result, err := billinganalysis.Build(r.Request.Context(), r.State.DB, mid, billinganalysis.Options{
		From: from, To: to, Provider: strings.TrimSpace(r.Query("provider")), Location: loc,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to build billing analysis")
		return
	}
	if status := strings.TrimSpace(r.Query("status")); status != "" {
		result.CurrentDelinquent = billinganalysis.FilterDelinquent(result.CurrentDelinquent, status)
	}
	r.JSON(http.StatusOK, result)
}

func parseBillingAnalysisTime(value string, loc *time.Location, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, loc)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}
