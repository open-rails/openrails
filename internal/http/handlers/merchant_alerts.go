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
	case db.IsNotFound(err), errors.Is(err, pgx.ErrNoRows):
		r.ErrorCode(billing.CodeResourceNotFound, "")
	case errors.Is(err, alerting.ErrWebhookRotationConflict):
		r.ErrorCode(billing.CodeResourceConflict, err.Error())
	default:
		writeRefusal(r, err, "alerting request failed")
	}
}

// ListAlertWebhooks handles GET /v1/merchant/alert-webhooks.
func ListAlertWebhooks(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	hooks, err := svc.ListWebhooks(r.Request.Context())
	if err != nil {
		r.InternalError("list alert webhooks failed", err)
		return
	}
	r.SuccessJSON(billing.ListPage[billing.AlertWebhook]{Items: hooks})
}

// CreateAlertWebhook handles POST /v1/merchant/alert-webhooks.
func CreateAlertWebhook(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	var in billing.CreateAlertWebhookRequest
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

// SetAlertWebhookURL handles PUT /v1/merchant/alert-webhooks/{id}/url: a new
// credential, the same webhook.
func SetAlertWebhookURL(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	id, ok := pathID(r, billing.ParseAlertWebhookID)
	if !ok {
		return
	}
	var in billing.SetAlertWebhookURLRequest
	if !r.BindJSON(&in) {
		return
	}
	hook, err := svc.SetWebhookURL(r.Request.Context(), id, in)
	if err != nil {
		handleAlertWriteError(r, err)
		return
	}
	r.SuccessJSON(hook)
}

// DeleteAlertWebhook handles DELETE /v1/merchant/alert-webhooks/{id}.
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

// ListMerchantNotificationsQuery is the inbox list's query.
type ListMerchantNotificationsQuery struct {
	Unread bool `form:"unread"`
}

// ListMerchantNotifications handles GET /v1/merchant/notifications.
func ListMerchantNotifications(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	var q ListMerchantNotificationsQuery
	if !r.BindQuery(&q) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	notes, err := svc.ListNotifications(r.Request.Context(), billing.ListMerchantNotificationsRequest{PageRequest: page, UnreadOnly: q.Unread})
	if err != nil {
		writeRefusal(r, err, "list notifications failed")
		return
	}
	r.SuccessJSON(notes)
}

// MarkMerchantNotificationRead handles POST /v1/merchant/notifications/{id}/read.
func MarkMerchantNotificationRead(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	id, ok := pathID(r, billing.ParseNotificationID)
	if !ok {
		return
	}
	note, err := svc.MarkNotificationRead(r.Request.Context(), id)
	if err != nil {
		handleAlertWriteError(r, err)
		return
	}
	r.SuccessJSON(note)
}

// MerchantNotificationsUnreadCount handles GET /v1/merchant/notifications/unread-count.
func MerchantNotificationsUnreadCount(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	count, err := svc.UnreadCount(r.Request.Context())
	if err != nil {
		r.InternalError("count unread notifications failed", err)
		return
	}
	r.SuccessJSON(billing.UnreadCount{UnreadCount: count})
}
