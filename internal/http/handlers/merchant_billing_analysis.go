package handlers

import (
	"net/http"
	"sort"
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
	now := r.Clock.Now().UTC()
	from, err := parseBillingAnalysisTime(r.Query("from"), loc, now.AddDate(0, 0, -30), false)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid from: use RFC3339 or YYYY-MM-DD")
		return
	}
	to, err := parseBillingAnalysisTime(r.Query("to"), loc, now, true)
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
	r.JSON(http.StatusOK, toBillingAnalysisResponse(result))
}

// billingAnalysisResponse is the merchant-console contract. The calculation
// package retains the full evidence report; the HTTP surface exposes the
// dashboard-sized daily series and the end-of-window open cases.
type billingAnalysisResponse struct {
	Range     billingAnalysisRange    `json:"range"`
	Timezone  string                  `json:"timezone"`
	Providers []string                `json:"providers"`
	Daily     []billingAnalysisDay    `json:"daily"`
	Unbilled  []billingAnalysisMember `json:"unbilled"`
}

type billingAnalysisRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type billingAnalysisDay struct {
	Date          string                  `json:"date"`
	Signups       int                     `json:"signups"`
	Rebills       int                     `json:"rebills"`
	FailedRebills int                     `json:"failed_rebills"`
	OpenUnbilled  int                     `json:"open_unbilled"`
	Unbilled      []billingAnalysisMember `json:"unbilled,omitempty"`
}

type billingAnalysisMember struct {
	ID             string `json:"id"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	Email          string `json:"email,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Amount         int64  `json:"amount,string"`
	Currency       string `json:"currency,omitempty"`
	UnbilledSince  string `json:"unbilled_since"`
	LastFailedAt   string `json:"last_failed_at"`
	FailureCount   int    `json:"failure_count"`
	FailureCode    string `json:"failure_code,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
	Status         string `json:"status"`
}

func toBillingAnalysisResponse(report *billinganalysis.Report) billingAnalysisResponse {
	if report == nil {
		return billingAnalysisResponse{Providers: []string{}, Daily: []billingAnalysisDay{}, Unbilled: []billingAnalysisMember{}}
	}
	providers := map[string]struct{}{}
	for _, c := range report.Charges {
		if c.Provider != "" {
			providers[c.Provider] = struct{}{}
		}
	}
	for _, f := range report.Failures {
		if f.Provider != "" {
			providers[f.Provider] = struct{}{}
		}
	}
	for _, d := range report.CurrentDelinquent {
		if d.Provider != "" {
			providers[d.Provider] = struct{}{}
		}
	}
	providerList := make([]string, 0, len(providers))
	for provider := range providers {
		providerList = append(providerList, provider)
	}
	sort.Strings(providerList)

	daily := make([]billingAnalysisDay, 0, len(report.Days))
	for _, d := range report.Days {
		day := billingAnalysisDay{
			Date: d.Date, Signups: d.SettledSignups, Rebills: d.SettledRebills,
			FailedRebills: d.FailedRebills, OpenUnbilled: d.OpenUnbilledUsers,
		}
		for _, c := range d.Unbilled {
			day.Unbilled = append(day.Unbilled, billingAnalysisMemberFromCase(c))
		}
		daily = append(daily, day)
	}
	var unbilled []billingAnalysisMember
	if len(report.Days) > 0 {
		last := report.Days[len(report.Days)-1]
		unbilled = make([]billingAnalysisMember, 0, len(last.Unbilled))
		for _, c := range last.Unbilled {
			unbilled = append(unbilled, billingAnalysisMemberFromCase(c))
		}
	}
	return billingAnalysisResponse{
		Range:    billingAnalysisRange{From: report.From, To: report.To},
		Timezone: report.Timezone, Providers: providerList, Daily: daily, Unbilled: unbilled,
	}
}

func billingAnalysisMemberFromCase(c billinganalysis.UnbilledCase) billingAnalysisMember {
	return billingAnalysisMember{
		ID: c.ObligationKey, SubscriptionID: c.SubscriptionRef,
		Email: c.CustomerEmail, Provider: c.Provider, Amount: c.AmountCents,
		Currency: c.Currency, UnbilledSince: c.UnbilledSince,
		LastFailedAt: c.LastFailureAt.UTC().Format(time.RFC3339),
		FailureCount: c.FailureCount, FailureCode: c.FailureCode,
		FailureReason: c.FailureReason, Status: "open",
	}
}

func parseBillingAnalysisTime(value string, loc *time.Location, fallback time.Time, endOfDay bool) (time.Time, error) {
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
	if endOfDay {
		return parsed.AddDate(0, 0, 1).Add(-time.Nanosecond), nil
	}
	return parsed, nil
}
