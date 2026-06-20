package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func GetMyUsage(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}
	from, to, ok := selfUsageWindow(r)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	rows, err := svc.GetUsage(r.Request.Context(), payer, currency, from, to)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(map[string]any{"usage": rows, "currency": currency, "from": from, "to": to})
}

func GetMyInvoices(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	limit, offset := selfLimitOffset(r, 50)
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	items, total, err := svc.ListInvoices(r.Request.Context(), payer, limit, offset)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(map[string]any{"invoices": items, "total": total, "limit": limit, "offset": offset})
}

func GetMyInvoice(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid invoice id")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	inv, err := svc.GetInvoice(r.Request.Context(), payer, id)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, err.Error())
		return
	}
	r.SuccessJSON(inv)
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

func selfUsageWindow(r *httprequest.Request) (time.Time, time.Time, bool) {
	to := time.Now().UTC()
	from := to.AddDate(0, -1, 0)
	if raw := r.Request.URL.Query().Get("from"); raw != "" {
		parsed, err := parseSelfTime(raw)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid from")
			return time.Time{}, time.Time{}, false
		}
		from = parsed
	}
	if raw := r.Request.URL.Query().Get("to"); raw != "" {
		parsed, err := parseSelfTime(raw)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid to")
			return time.Time{}, time.Time{}, false
		}
		to = parsed
	}
	if !from.Before(to) {
		r.ErrorJSON(http.StatusBadRequest, "from must be before to")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

func parseSelfTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", raw)
}
