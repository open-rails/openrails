package handlers

import (
	"encoding/json"
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
	if to.Before(from) {
		r.ErrorJSON(http.StatusBadRequest, "invalid billing analysis range: to precedes from")
		return
	}
	if billingAnalysisCalendarDays(from, to, loc) > 366 {
		r.ErrorJSON(http.StatusBadRequest, "billing analysis range cannot exceed 366 days")
		return
	}
	result, err := billinganalysis.Build(r.Request.Context(), r.State.DB, mid, billinganalysis.Options{
		From: from, To: to, Provider: strings.TrimSpace(r.Query("provider")), Location: loc,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to build billing analysis")
		return
	}
	r.JSON(http.StatusOK, toBillingAnalysisResponse(result))
}

// billingAnalysisResponse is the merchant-console contract. The calculation
// package retains the full evidence report; the HTTP surface exposes the
// dashboard-sized daily series and the end-of-window open cases.
type billingAnalysisResponse struct {
	Range      billingAnalysisRange        `json:"range"`
	Timezone   string                      `json:"timezone"`
	Providers  []string                    `json:"providers"`
	Daily      []billingAnalysisDay        `json:"daily"`
	Unbilled   []billingAnalysisMember     `json:"unbilled"`
	Delinquent []billingAnalysisDelinquent `json:"delinquent"`
}

type billingAnalysisRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type billingAnalysisDay struct {
	Date            string                   `json:"date"`
	Signups         int                      `json:"signups"`
	Rebills         int                      `json:"rebills"`
	SettledOther    int                      `json:"settled_other"`
	FailedSignups   int                      `json:"failed_signups"`
	FailedRebills   int                      `json:"failed_rebills"`
	FailedOther     int                      `json:"failed_other"`
	OpenUnbilled    int                      `json:"open_unbilled"`
	DelinquentUsers int                      `json:"delinquent_users"`
	Charges         []billingAnalysisCharge  `json:"charges"`
	Failures        []billingAnalysisFailure `json:"failures"`
	Unbilled        []billingAnalysisMember  `json:"unbilled,omitempty"`
}

type billingAnalysisMember struct {
	ID             string `json:"id"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	CustomerRef    string `json:"customer_ref,omitempty"`
	Email          string `json:"email,omitempty"`
	Provider       string `json:"provider,omitempty"`
	AmountCents    int64  `json:"amount_cents,string"`
	Currency       string `json:"currency,omitempty"`
	UnbilledSince  string `json:"unbilled_since"`
	LastFailedAt   string `json:"last_failed_at"`
	FailureCount   int    `json:"failure_count"`
	FailureCode    string `json:"failure_code,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
	Status         string `json:"status"`
}

// Provider references and raw payloads are retained for investigation. They
// are never interpreted as local customer IDs or turned into customer links.
type billingAnalysisCharge struct {
	Day             string          `json:"day"`
	Kind            string          `json:"kind"`
	Provider        string          `json:"provider"`
	PSPID           string          `json:"psp_id,omitempty"`
	EventKey        string          `json:"event_key"`
	TransactionID   string          `json:"transaction_id,omitempty"`
	SubscriptionRef string          `json:"subscription_ref,omitempty"`
	CustomerRef     string          `json:"customer_ref,omitempty"`
	CustomerEmail   string          `json:"customer_email,omitempty"`
	OrderRef        string          `json:"order_ref,omitempty"`
	AmountCents     int64           `json:"amount_cents,string"`
	Currency        string          `json:"currency,omitempty"`
	OccurredAt      time.Time       `json:"occurred_at"`
	Source          string          `json:"source,omitempty"`
	Raw             json.RawMessage `json:"raw,omitempty"`
}

type billingAnalysisFailure struct {
	billingAnalysisCharge
	DeclineCode   string `json:"decline_code,omitempty"`
	DeclineReason string `json:"decline_reason,omitempty"`
	FailureCount  int    `json:"failure_count"`
}

type billingAnalysisDelinquent struct {
	Provider        string     `json:"provider"`
	PSPID           string     `json:"psp_id,omitempty"`
	SubscriptionRef string     `json:"subscription_ref,omitempty"`
	CustomerRef     string     `json:"customer_ref,omitempty"`
	CustomerEmail   string     `json:"customer_email,omitempty"`
	Status          string     `json:"status,omitempty"`
	NextBillingAt   *time.Time `json:"next_billing_at,omitempty"`
	ObligationKey   string     `json:"obligation_key,omitempty"`
}

func toBillingAnalysisResponse(report *billinganalysis.Report) billingAnalysisResponse {
	if report == nil {
		return billingAnalysisResponse{Providers: []string{}, Daily: []billingAnalysisDay{}, Unbilled: []billingAnalysisMember{}, Delinquent: []billingAnalysisDelinquent{}}
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
			SettledOther: d.SettledOther, FailedSignups: d.FailedSignups,
			FailedRebills: d.FailedRebills, FailedOther: d.FailedOther,
			OpenUnbilled: d.OpenUnbilledUsers, DelinquentUsers: d.DelinquentUsers,
			Charges: []billingAnalysisCharge{}, Failures: []billingAnalysisFailure{},
		}
		for _, c := range d.Unbilled {
			day.Unbilled = append(day.Unbilled, billingAnalysisMemberFromCase(c))
		}
		daily = append(daily, day)
	}
	byDay := make(map[string]*billingAnalysisDay, len(daily))
	for i := range daily {
		byDay[daily[i].Date] = &daily[i]
	}
	for _, c := range report.Charges {
		if day := byDay[c.Day]; day != nil {
			day.Charges = append(day.Charges, billingAnalysisChargeFrom(c))
		}
	}
	for _, f := range report.Failures {
		if day := byDay[f.Day]; day != nil {
			day.Failures = append(day.Failures, billingAnalysisFailureFrom(f))
		}
	}
	var unbilled []billingAnalysisMember
	if len(report.Days) > 0 {
		last := report.Days[len(report.Days)-1]
		unbilled = make([]billingAnalysisMember, 0, len(last.Unbilled))
		for _, c := range last.Unbilled {
			unbilled = append(unbilled, billingAnalysisMemberFromCase(c))
		}
	}
	delinquent := make([]billingAnalysisDelinquent, 0, len(report.CurrentDelinquent))
	for _, d := range report.CurrentDelinquent {
		delinquent = append(delinquent, billingAnalysisDelinquent{
			Provider: d.Provider, PSPID: d.PSPID, SubscriptionRef: d.SubscriptionRef,
			CustomerRef: d.CustomerRef, CustomerEmail: d.CustomerEmail, Status: d.Status,
			NextBillingAt: d.NextBillingAt, ObligationKey: d.ObligationKey,
		})
	}
	sort.Slice(delinquent, func(i, j int) bool { return delinquent[i].ObligationKey < delinquent[j].ObligationKey })
	return billingAnalysisResponse{
		Range: billingAnalysisRange{From: report.From, To: report.To}, Timezone: report.Timezone,
		Providers: providerList, Daily: daily, Unbilled: unbilled, Delinquent: delinquent,
	}
}

func billingAnalysisMemberFromCase(c billinganalysis.UnbilledCase) billingAnalysisMember {
	return billingAnalysisMember{
		ID: c.ObligationKey, SubscriptionID: c.SubscriptionRef,
		CustomerRef: c.CustomerRef, Email: c.CustomerEmail, Provider: c.Provider, AmountCents: c.AmountCents,
		Currency: c.Currency, UnbilledSince: c.UnbilledSince,
		LastFailedAt: c.LastFailureAt.UTC().Format(time.RFC3339),
		FailureCount: c.FailureCount, FailureCode: c.FailureCode,
		FailureReason: c.FailureReason, Status: "open",
	}
}

func billingAnalysisChargeFrom(c billinganalysis.Charge) billingAnalysisCharge {
	return billingAnalysisCharge{Day: c.Day, Kind: c.Kind, Provider: c.Provider, PSPID: c.PSPID,
		EventKey: c.EventKey, TransactionID: c.TransactionID, SubscriptionRef: c.SubscriptionRef,
		CustomerRef: c.CustomerRef, CustomerEmail: c.CustomerEmail, OrderRef: c.OrderRef,
		AmountCents: c.AmountCents, Currency: c.Currency, OccurredAt: c.OccurredAt,
		Source: c.Source, Raw: c.Raw}
}

func billingAnalysisFailureFrom(f billinganalysis.Failure) billingAnalysisFailure {
	return billingAnalysisFailure{billingAnalysisCharge: billingAnalysisChargeFrom(f.Charge),
		DeclineCode: f.DeclineCode, DeclineReason: f.DeclineReason, FailureCount: f.FailureCount}
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

func billingAnalysisCalendarDays(from, to time.Time, loc *time.Location) int {
	start := from.In(loc)
	end := to.In(loc)
	startDate := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	endDate := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	return int(endDate.Sub(startDate)/(24*time.Hour)) + 1
}
