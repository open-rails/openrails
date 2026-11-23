package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/service"
)

func ListEvents(r *httprequest.Request) {
	var query billing.EventQuery
	if !r.BindQuery(&query) {
		return
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
