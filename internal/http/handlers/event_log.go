package handlers

import (
	"strconv"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/service"
)

type EventLogQuery struct {
	After string   `form:"after"`
	Limit int      `form:"limit"`
	Types []string `form:"type"`
}

func ListEvents(r *httprequest.Request) {
	query := billing.EventQuery{After: r.Query("after"), Types: r.Request.URL.Query()["type"]}
	if raw := r.Query("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "invalid limit").WithParam("limit"))
			return
		}
		query.Limit = value
	}
	svc, err := service.New(r.State)
	if err != nil {
		writeRefusal(r, err, "events could not be listed")
		return
	}
	page, err := svc.ListEvents(r.Request.Context(), query)
	if err != nil {
		writeRefusal(r, err, "events could not be listed")
		return
	}
	r.SuccessJSON(page)
}
