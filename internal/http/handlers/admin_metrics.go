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

func GetAdminMetricsSummary(r *httprequest.Request) {
	rng, err := parseMetricsRange(r, 30)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	currency := strings.TrimSpace(r.Query("currency"))
	svc := analytics.NewAdminMetricsService(r.State.Config.ClickHouse)
	resp, err := svc.GetSummary(r.Request.Context(), rng, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	if len(resp) == 1 {
		r.SuccessJSON(resp[0])
		return
	}
	if currency == "" && len(resp) > 1 {
		r.ErrorJSON(http.StatusBadRequest, "multiple currencies present; specify ?currency=XXX")
		return
	}
	r.SuccessJSON(resp)
}

func GetAdminMetricsRevenue(r *httprequest.Request) {
	rng, err := parseMetricsRange(r, 30)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	granularity := r.Query("granularity")
	currency := strings.TrimSpace(r.Query("currency"))
	svc := analytics.NewAdminMetricsService(r.State.Config.ClickHouse)
	resp, err := svc.GetRevenueSeries(r.Request.Context(), rng, granularity, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	if len(resp) == 1 {
		r.SuccessJSON(resp[0])
		return
	}
	if currency == "" && len(resp) > 1 {
		r.ErrorJSON(http.StatusBadRequest, "multiple currencies present; specify ?currency=XXX")
		return
	}
	r.SuccessJSON(resp)
}

func GetAdminMetricsSubscriptions(r *httprequest.Request) {
	rng, err := parseMetricsRange(r, 30)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	granularity := r.Query("granularity")
	currency := strings.TrimSpace(r.Query("currency"))
	svc := analytics.NewAdminMetricsService(r.State.Config.ClickHouse)
	resp, err := svc.GetSubscriptionSeries(r.Request.Context(), rng, granularity, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	if len(resp) == 1 {
		r.SuccessJSON(resp[0])
		return
	}
	if currency == "" && len(resp) > 1 {
		r.ErrorJSON(http.StatusBadRequest, "multiple currencies present; specify ?currency=XXX")
		return
	}
	r.SuccessJSON(resp)
}

func GetAdminMetricsProcessors(r *httprequest.Request) {
	rng, err := parseMetricsRange(r, 30)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	currency := strings.TrimSpace(r.Query("currency"))
	svc := analytics.NewAdminMetricsService(r.State.Config.ClickHouse)
	resp, err := svc.GetProcessorMetrics(r.Request.Context(), rng, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	if len(resp) == 1 {
		r.SuccessJSON(resp[0])
		return
	}
	if currency == "" && len(resp) > 1 {
		r.ErrorJSON(http.StatusBadRequest, "multiple currencies present; specify ?currency=XXX")
		return
	}
	r.SuccessJSON(resp)
}

func GetAdminMetricsChurn(r *httprequest.Request) {
	rng, err := parseMetricsRange(r, 180)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	currency := strings.TrimSpace(r.Query("currency"))
	svc := analytics.NewAdminMetricsService(r.State.Config.ClickHouse)
	resp, err := svc.GetChurn(r.Request.Context(), rng, currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	if len(resp) == 1 {
		r.SuccessJSON(resp[0])
		return
	}
	if currency == "" && len(resp) > 1 {
		r.ErrorJSON(http.StatusBadRequest, "multiple currencies present; specify ?currency=XXX")
		return
	}
	r.SuccessJSON(resp)
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
