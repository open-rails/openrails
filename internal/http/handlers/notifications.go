package handlers

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/query"
)

func GetNotifications(r *httprequest.Request) {
	user := r.GetUser()
	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	var seen *bool
	switch r.Request.URL.Query().Get("seen") {
	case "true":
		v := true
		seen = &v
	case "false":
		v := false
		seen = &v
	}

	q := &query.QueryOptions[subscriptions.GetNotificationsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: subscriptions.GetNotificationsFilters{UserID: user.ID, Seen: seen},
	}
	items, _, err := r.State.UserSubscriptionService.GetUserNotifications(r.Request.Context(), user.ID, q)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve notifications")
		return
	}
	r.SuccessJSONPaginated(notificationViews(items), q.TotalItems, limit, offset)
}

func notificationViews(items []*models.NotificationQueue) []openrails.Notification {
	out := make([]openrails.Notification, 0, len(items))
	for _, n := range items {
		out = append(out, n.View())
	}
	return out
}

func MarkNotificationRead(r *httprequest.Request) {
	user := r.GetUser()
	id, err := uuid.Parse(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid notification ID")
		return
	}
	if err := r.State.UserSubscriptionService.MarkNotificationRead(r.Request.Context(), user.ID, id); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to mark notification read")
		return
	}
	r.SuccessJSONMessage("notification marked as read")
}

func GetUnreadNotificationCount(r *httprequest.Request) {
	user := r.GetUser()
	seen := false
	q := &query.QueryOptions[subscriptions.GetNotificationsFilters]{
		Limit:   1,
		Offset:  0,
		Filters: subscriptions.GetNotificationsFilters{UserID: user.ID, Seen: &seen},
	}
	if _, _, err := r.State.UserSubscriptionService.GetUserNotifications(r.Request.Context(), user.ID, q); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve notifications")
		return
	}
	r.SuccessJSON(map[string]any{"unread_count": q.TotalItems})
}
