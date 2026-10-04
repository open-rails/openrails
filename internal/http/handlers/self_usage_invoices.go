package handlers

import (
	"strconv"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// GetMyUsage reports the customer's own usage.
func GetMyUsage(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	getUsage(r, payer)
}

func selfLimitOffset(r *httprequest.Request, def int) (int, int) {
	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = def
	}
	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// usageWindow reads ?from= and ?to= (RFC 3339 or a date); the default is
// the month before now.
func usageWindow(r *httprequest.Request) (time.Time, time.Time, bool) {
	to := r.Clock.Now().UTC()
	from := to.AddDate(0, -1, 0)
	for _, bound := range []struct {
		name string
		into *time.Time
	}{{"from", &from}, {"to", &to}} {
		raw := r.Request.URL.Query().Get(bound.name)
		if raw == "" {
			continue
		}
		parsed, err := parseSelfTime(raw)
		if err != nil {
			r.APIError(api.Coded(billing.CodeInvalidQuery, bound.name+" is not a time").WithParam(bound.name))
			return time.Time{}, time.Time{}, false
		}
		*bound.into = parsed.UTC()
	}
	if !from.Before(to) {
		r.APIError(api.Coded(billing.CodeInvalidQuery, "from must be before to").WithParam("from"))
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// getUsage answers a customer's usage report.
func getUsage(r *httprequest.Request, customer billing.CustomerID) {
	from, to, ok := usageWindow(r)
	if !ok {
		return
	}
	query := r.Request.URL.Query()
	params := billing.UsageParams{Currency: query.Get("currency"), From: from, To: to, GroupBy: billing.UsageGroupBy(query.Get("group_by"))}
	switch params.GroupBy {
	case "", billing.UsageByEventType, billing.UsageByResource, billing.UsageByInvoker, billing.UsageByFunction, billing.UsageByTier:
	default:
		r.APIError(api.Coded(billing.CodeInvalidQuery, "group_by must be event_type, resource, invoker, function or tier").WithParam("group_by"))
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
