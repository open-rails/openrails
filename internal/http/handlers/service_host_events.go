package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	service "github.com/open-rails/openrails/internal/service"
)

func writeHostEventError(r *httprequest.Request, err error) {
	var status *openrails.StatusError
	if errors.As(err, &status) {
		r.JSON(status.Status, map[string]any{"error": status.ErrorDetails})
		return
	}
	r.ErrorJSON(http.StatusInternalServerError, "host event operation failed")
}

func ServiceListHostEvents(r *httprequest.Request) {
	svc, err := service.New(r.State)
	if err != nil {
		writeHostEventError(r, err)
		return
	}
	options := openrails.HostEventListOptions{Type: openrails.HostEventType(r.Query("type"))}
	if value := r.Query("limit"); value != "" {
		options.Limit, err = strconv.Atoi(value)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid limit")
			return
		}
	}
	if value := r.Query("include_acknowledged"); value != "" {
		options.IncludeAcknowledged, err = strconv.ParseBool(value)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid include_acknowledged")
			return
		}
	}
	if value := r.Query("payment_id"); value != "" {
		options.PaymentID, err = uuid.Parse(value)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid payment_id")
			return
		}
	}
	events, err := svc.ListHostEvents(r.Request.Context(), options)
	if err != nil {
		writeHostEventError(r, err)
		return
	}
	r.JSON(http.StatusOK, events)
}

func ServiceAcknowledgeHostEvent(r *httprequest.Request) {
	id, err := uuid.Parse(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid host event id")
		return
	}
	svc, err := service.New(r.State)
	if err != nil {
		writeHostEventError(r, err)
		return
	}
	if err := svc.AcknowledgeHostEvent(r.Request.Context(), id); err != nil {
		writeHostEventError(r, err)
		return
	}
	r.JSON(http.StatusOK, map[string]bool{"acknowledged": true})
}
