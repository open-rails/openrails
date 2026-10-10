package handlers

// Merchant alert webhooks and the notification inbox.

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/alerting"
)

func alertService(r *httprequest.Request) (*alerting.Service, bool) {
	if r.State == nil || r.State.AlertService == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "alerting is not configured")
		return nil, false
	}
	return r.State.AlertService, true
}

// pathID reads the {id} path parameter as a typed id: 400 invalid_param when
// it is not one.
func pathID[T interface{ IsZero() bool }](r *httprequest.Request, parse func(string) (T, error)) (T, bool) {
	id, err := parse(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "id is invalid").WithParam("id"))
		var zero T
		return zero, false
	}
	return id, true
}

func alertValidationError(r *httprequest.Request, verr *alerting.ValidationError) {
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "webhook_invalid", verr.Error()).
		WithMetadata(map[string]any{"errors": verr.Errors}))
}

// handleAlertWriteError maps service errors to API errors (validation → 400,
// missing row → 404, typed capability refusals → their status, else 500).
func handleAlertWriteError(r *httprequest.Request, err error) {
	var verr *alerting.ValidationError
	switch {
	case errors.As(err, &verr):
		alertValidationError(r, verr)
	case errors.Is(err, alerting.ErrWebhookNotFound), db.IsNotFound(err), errors.Is(err, pgx.ErrNoRows):
		r.ErrorCode(billing.CodeResourceNotFound, "")
	default:
		writeRefusal(r, err, "alerting request failed")
	}
}

// ListAlertWebhooks handles GET /v1/admin/alert-webhooks.
func ListAlertWebhooks(r *httprequest.Request) {
	ids, ok := listIDs(r, billing.ParseAlertWebhookID)
	if !ok {
		return
	}
	svc, ok := alertService(r)
	if !ok {
		return
	}
	hooks, err := svc.ListWebhooks(r.Request.Context(), billing.AlertWebhookListParams{IDs: ids})
	if err != nil {
		r.InternalError("list alert webhooks failed", err)
		return
	}
	r.SuccessJSON(billing.ListPage[billing.AlertWebhook]{Items: hooks})
}

// CreateAlertWebhook handles POST /v1/admin/alert-webhooks.
func CreateAlertWebhook(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	var in billing.CreateAlertWebhookParams
	if !r.BindJSON(&in) {
		return
	}
	hook, err := svc.CreateWebhook(r.Request.Context(), in)
	if err != nil {
		handleAlertWriteError(r, err)
		return
	}
	r.JSON(http.StatusCreated, hook)
}

// UpdateAlertWebhook handles PATCH /v1/admin/alert-webhooks/{id}.
func UpdateAlertWebhook(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	id, ok := pathID(r, billing.ParseAlertWebhookID)
	if !ok {
		return
	}
	var in billing.UpdateAlertWebhookParams
	if !r.BindJSON(&in) {
		return
	}
	hook, err := svc.UpdateWebhook(r.Request.Context(), id, in)
	if err != nil {
		handleAlertWriteError(r, err)
		return
	}
	r.SuccessJSON(hook)
}

// DeleteAlertWebhook handles DELETE /v1/admin/alert-webhooks/{id}.
func DeleteAlertWebhook(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	id, ok := pathID(r, billing.ParseAlertWebhookID)
	if !ok {
		return
	}
	deleted, err := svc.DeleteWebhook(r.Request.Context(), id)
	if err != nil {
		handleAlertWriteError(r, err)
		return
	}
	if !deleted {
		r.ErrorCode(billing.CodeResourceNotFound, "")
		return
	}
	r.NoContent()
}
