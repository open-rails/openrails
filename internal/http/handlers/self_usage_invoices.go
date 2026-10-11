package handlers

import (
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// GetMyUsage reports the customer's own usage.
func GetMyUsage(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	getUsage(r, payer)
}

// getUsage answers a page of a customer's usage report. ?from= and ?to= are
// RFC 3339 or a date; left out, the cursor's window or the month before now.
func getUsage(r *httprequest.Request, customer billing.CustomerID) {
	query := r.Request.URL.Query()
	params := billing.GetUsageParams{Currency: query.Get("currency"), GroupBy: billing.UsageGroupBy(query.Get("group_by"))}
	for _, bound := range []struct {
		name string
		into *time.Time
	}{{"from", &params.From}, {"to", &params.To}} {
		raw := query.Get(bound.name)
		if raw == "" {
			continue
		}
		parsed, err := parseSelfTime(raw)
		if err != nil {
			r.APIError(api.Coded(billing.CodeInvalidQuery, bound.name+" is not a time").WithParam(bound.name))
			return
		}
		*bound.into = parsed
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

func parseSelfTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", raw)
}
