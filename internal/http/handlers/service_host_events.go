package handlers

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	service "github.com/open-rails/openrails/internal/service"
)

func writeHostEventError(r *httprequest.Request, err error) {
	writeRefusal(r, err, "host event operation failed")
}

// HostEventsQuery is the host event list's filters.
type HostEventsQuery struct {
	Type                string `form:"type"`
	IncludeAcknowledged bool   `form:"include_acknowledged"`
	PaymentID           string `form:"payment_id"`
}

// ServiceListHostEvents handles GET /v1/merchant/host-events.
func ServiceListHostEvents(r *httprequest.Request) {
	var q HostEventsQuery
	if !r.BindQuery(&q) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	req := billing.HostEventListParams{PageRequest: page, Type: billing.HostEventType(q.Type), IncludeAcknowledged: q.IncludeAcknowledged}
	if q.PaymentID != "" {
		id, err := billing.ParsePaymentID(q.PaymentID)
		if err != nil || id.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "payment_id is invalid").WithParam("payment_id"))
			return
		}
		req.PaymentID = id
	}
	svc, err := service.New(r.State)
	if err != nil {
		writeHostEventError(r, err)
		return
	}
	events, err := svc.ListHostEvents(r.Request.Context(), req)
	if err != nil {
		writeHostEventError(r, err)
		return
	}
	r.SuccessJSON(events)
}

// ServiceAcknowledgeHostEvent handles POST /v1/merchant/host-events/{id}/acknowledge.
func ServiceAcknowledgeHostEvent(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParseHostEventID)
	if !ok {
		return
	}
	svc, err := service.New(r.State)
	if err != nil {
		writeHostEventError(r, err)
		return
	}
	event, err := svc.AcknowledgeHostEvent(r.Request.Context(), id)
	if err != nil {
		writeHostEventError(r, err)
		return
	}
	r.SuccessJSON(event)
}
