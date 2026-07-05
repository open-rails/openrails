package handlers

// Metric threshold alerting (#736): merchant-operator CRUD for alert rules and
// outbound webhooks, the in_app notification bell, and per-rule test-fire. Reads
// share the metrics-read permission; mutations are settings-write gated (routes).

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/pkg/api"
)

func alertService(r *httprequest.Request) (*alerting.Service, bool) {
	if r.State == nil || r.State.AlertService == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "alerting service not configured")
		return nil, false
	}
	return r.State.AlertService, true
}

func alertPathID(r *httprequest.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil {
		r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found", "no record with that id in this merchant"))
		return uuid.Nil, false
	}
	return id, true
}

func alertValidationError(r *httprequest.Request, verr *alerting.ValidationError) {
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "alert_rule_invalid", verr.Error()).
		WithMetadata(map[string]any{"errors": verr.Errors}))
}

// handleAlertWriteError maps service errors to API errors (validation → 400,
// missing row → 404, else 500).
func handleAlertWriteError(r *httprequest.Request, err error, notFoundMsg string) {
	var verr *alerting.ValidationError
	switch {
	case errors.As(err, &verr):
		alertValidationError(r, verr)
	case db.IsNotFound(err):
		r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found", notFoundMsg))
	default:
		r.ErrorJSON(http.StatusInternalServerError, "alerting request failed")
	}
}

// --- rules -------------------------------------------------------------------

// AlertRuleTemplates handles GET /v1/merchant/alerts/templates: the in-code
// template registry (key, params, defaults) the console renders create/edit
// forms from. Additive to the pinned contract (documented in #736) so the
// console never hardcodes param specs that would drift from the engine.
func AlertRuleTemplates(r *httprequest.Request) {
	r.JSON(http.StatusOK, map[string]any{"data": alerting.Templates()})
}

// ListAlertRules handles GET /v1/merchant/alerts/rules.
func ListAlertRules(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	rules, err := svc.ListRules(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list alert rules")
		return
	}
	r.JSON(http.StatusOK, map[string]any{"data": rules})
}

// CreateAlertRule handles POST /v1/merchant/alerts/rules.
func CreateAlertRule(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	var in alerting.CreateRuleInput
	if !r.BindJSON(&in) {
		return
	}
	rule, err := svc.CreateRule(r.Request.Context(), in)
	if err != nil {
		handleAlertWriteError(r, err, "alert rule not found")
		return
	}
	r.JSON(http.StatusCreated, rule)
}

// UpdateAlertRule handles PATCH /v1/merchant/alerts/rules/{id}.
func UpdateAlertRule(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	id, ok := alertPathID(r)
	if !ok {
		return
	}
	var in alerting.UpdateRuleInput
	if !r.BindJSON(&in) {
		return
	}
	rule, err := svc.UpdateRule(r.Request.Context(), id, in)
	if err != nil {
		handleAlertWriteError(r, err, "no alert rule with that id in this merchant")
		return
	}
	r.JSON(http.StatusOK, rule)
}

// DeleteAlertRule handles DELETE /v1/merchant/alerts/rules/{id}.
func DeleteAlertRule(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	id, ok := alertPathID(r)
	if !ok {
		return
	}
	deleted, err := svc.DeleteRule(r.Request.Context(), id)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to delete alert rule")
		return
	}
	if !deleted {
		r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found", "no alert rule with that id in this merchant"))
		return
	}
	r.JSON(http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// TestFireAlertRule handles POST /v1/merchant/alerts/rules/{id}/test: sends a
// clearly-marked TEST alert through the rule's real channels (no edge-state
// change) and returns the per-channel delivery results.
func TestFireAlertRule(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	id, ok := alertPathID(r)
	if !ok {
		return
	}
	results, err := svc.TestFireRule(r.Request.Context(), id)
	if err != nil {
		if db.IsNotFound(err) {
			r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found", "no alert rule with that id in this merchant"))
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to test-fire alert rule")
		return
	}
	r.JSON(http.StatusOK, map[string]any{"results": results})
}

// --- webhooks ----------------------------------------------------------------

// ListMerchantWebhooks handles GET /v1/merchant/webhooks.
func ListMerchantWebhooks(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	hooks, err := svc.ListWebhooks(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list webhooks")
		return
	}
	r.JSON(http.StatusOK, map[string]any{"data": hooks})
}

// CreateMerchantWebhook handles POST /v1/merchant/webhooks.
func CreateMerchantWebhook(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	var in alerting.CreateWebhookInput
	if !r.BindJSON(&in) {
		return
	}
	hook, err := svc.CreateWebhook(r.Request.Context(), in)
	if err != nil {
		handleAlertWriteError(r, err, "webhook not found")
		return
	}
	r.JSON(http.StatusCreated, hook)
}

// DeleteMerchantWebhook handles DELETE /v1/merchant/webhooks/{id}.
func DeleteMerchantWebhook(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	id, ok := alertPathID(r)
	if !ok {
		return
	}
	deleted, err := svc.DeleteWebhook(r.Request.Context(), id)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to delete webhook")
		return
	}
	if !deleted {
		r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found", "no webhook with that id in this merchant"))
		return
	}
	r.JSON(http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// --- notifications (bell) ----------------------------------------------------

// ListMerchantNotifications handles GET /v1/merchant/notifications?unread=.
func ListMerchantNotifications(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	unreadOnly := isTruthy(r.Query("unread"))
	notes, err := svc.ListNotifications(r.Request.Context(), unreadOnly)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list notifications")
		return
	}
	r.JSON(http.StatusOK, map[string]any{"data": notes})
}

// MarkMerchantNotificationRead handles POST /v1/merchant/notifications/{id}/read.
func MarkMerchantNotificationRead(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	id, ok := alertPathID(r)
	if !ok {
		return
	}
	marked, err := svc.MarkNotificationRead(r.Request.Context(), id)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to mark notification read")
		return
	}
	if !marked {
		r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "not_found", "no notification with that id in this merchant"))
		return
	}
	r.JSON(http.StatusOK, map[string]any{"read": true, "id": id})
}

// MerchantNotificationsUnreadCount handles GET /v1/merchant/notifications/unread-count.
func MerchantNotificationsUnreadCount(r *httprequest.Request) {
	svc, ok := alertService(r)
	if !ok {
		return
	}
	count, err := svc.UnreadCount(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to count unread notifications")
		return
	}
	r.JSON(http.StatusOK, map[string]any{"unread": count})
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
