package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Admin metrics handler (issue #232; #528 folded the 5 sub-routes into one).
//
// GET /admin/metrics is the PER-MERCHANT admin dashboard surface. AdminMetricsService
// resolves the merchant id from the request context (merchant.Require) and pins
// every ClickHouse query to WHERE merchant_id = ?, so a merchant operator can only
// ever read their OWN metrics — never cross-tenant (that is the separate
// platform-superadmin path).
//
// Query params control the shape: ?period/?start/?end (date range), ?granularity
// (revenue + subscription series), ?currency (disambiguates multi-currency
// merchants — required when more than one currency is present, else that section
// would be ambiguous). ClickHouse is OPTIONAL and DERIVED: Postgres is the system
// of record; a ClickHouse outage degrades this dashboard but never blocks openrails.

// GetAdminMetrics returns the merchant's billing analytics in one response:
// {summary, revenue, subscriptions, processors, churn}.
func GetAdminMetrics(r *httprequest.Request) {
	rng, err := parseMetricsRange(r, 30)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	currency := strings.TrimSpace(r.Query("currency"))
	granularity := r.Query("granularity")
	ctx := r.Request.Context()
	svc := analytics.NewAdminMetricsService(r.State.Config.ClickHouse)

	summary, err := svc.GetSummary(ctx, rng, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	revenue, err := svc.GetRevenueSeries(ctx, rng, granularity, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	subscriptions, err := svc.GetSubscriptionSeries(ctx, rng, granularity, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	processors, err := svc.GetProcessorMetrics(ctx, rng, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	churn, err := svc.GetChurn(ctx, rng, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	out := map[string]any{}
	var ok bool
	if out["summary"], ok = resolveMetricSection(r, summary, currency); !ok {
		return
	}
	if out["revenue"], ok = resolveMetricSection(r, revenue, currency); !ok {
		return
	}
	if out["subscriptions"], ok = resolveMetricSection(r, subscriptions, currency); !ok {
		return
	}
	if out["processors"], ok = resolveMetricSection(r, processors, currency); !ok {
		return
	}
	if out["churn"], ok = resolveMetricSection(r, churn, currency); !ok {
		return
	}
	r.SuccessJSON(out)
}

// resolveMetricSection applies the multi-currency rule for one metric section: a
// single-currency merchant gets the bare object; a multi-currency merchant with
// ?currency set gets that currency's object; a multi-currency merchant with no
// ?currency is ambiguous → 400 (and the handler aborts). Returns (section, ok).
func resolveMetricSection[T any](r *httprequest.Request, resp []T, currency string) (any, bool) {
	if len(resp) == 1 {
		return resp[0], true
	}
	if currency == "" && len(resp) > 1 {
		r.ErrorJSON(http.StatusBadRequest, "multiple currencies present; specify ?currency=XXX")
		return nil, false
	}
	return resp, true
}

func parseMetricsRange(r *httprequest.Request, defaultDays int) (analytics.MetricsDateRange, error) {
	startParam := strings.TrimSpace(r.Query("start"))
	endParam := strings.TrimSpace(r.Query("end"))
	periodParam := strings.TrimSpace(r.Query("period"))

	now := time.Now().UTC()
	var start, end time.Time
	var err error

	if endParam != "" {
		end, err = parseDateParam(endParam)
		if err != nil {
			return analytics.MetricsDateRange{}, err
		}
	} else {
		end = now
	}

	if startParam != "" {
		start, err = parseDateParam(startParam)
		if err != nil {
			return analytics.MetricsDateRange{}, err
		}
	} else if periodParam != "" {
		start = applyPeriod(end, periodParam)
	} else {
		start = end.AddDate(0, 0, -defaultDays)
	}

	if start.After(end) {
		start, end = end, start
	}

	return analytics.MetricsDateRange{Start: start, End: end}, nil
}

func parseDateParam(value string) (time.Time, error) {
	parsed, err := timeutil.ParseDateOrRFC3339UTC(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date: %s", value)
	}
	return parsed, nil
}

func applyPeriod(end time.Time, period string) time.Time {
	lower := strings.ToLower(period)
	switch lower {
	case "7d":
		return end.AddDate(0, 0, -7)
	case "30d", "":
		return end.AddDate(0, 0, -30)
	case "90d":
		return end.AddDate(0, 0, -90)
	case "month":
		return end.AddDate(0, -1, 0)
	case "quarter":
		return end.AddDate(0, -3, 0)
	case "year":
		return end.AddDate(-1, 0, 0)
	default:
		if strings.HasSuffix(lower, "d") {
			days, err := strconv.Atoi(strings.TrimSuffix(lower, "d"))
			if err == nil {
				return end.AddDate(0, 0, -days)
			}
		}
		return end.AddDate(0, 0, -30)
	}
}
