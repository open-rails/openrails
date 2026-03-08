package handlers

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
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

	seenParam := r.Request.URL.Query().Get("seen")
	var seen *bool
	if seenParam == "true" {
		t := true
		seen = &t
	}
	if seenParam == "false" {
		f := false
		seen = &f
	}

	q := &query.QueryOptions[services.GetNotificationsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: services.GetNotificationsFilters{UserID: user.ID, Seen: seen},
	}

	items, _, err := r.State.UserSubscriptionService.GetUserNotifications(r.Request.Context(), user.ID, q)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	r.SuccessJSONPaginated(items, q.TotalItems, limit, offset)
}

func MarkNotificationRead(r *httprequest.Request) {
	user := r.GetUser()
	idStr := r.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid notification ID")
		return
	}
	if err := r.State.UserSubscriptionService.MarkNotificationRead(r.Request.Context(), user.ID, id); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSONMessage("notification marked as read")
}

func GetUnreadNotificationCount(r *httprequest.Request) {
	user := r.GetUser()
	f := false
	q := &query.QueryOptions[services.GetNotificationsFilters]{
		Limit:   1,
		Offset:  0,
		Filters: services.GetNotificationsFilters{UserID: user.ID, Seen: &f},
	}
	if _, _, err := r.State.UserSubscriptionService.GetUserNotifications(r.Request.Context(), user.ID, q); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSON(map[string]any{"unread_count": q.TotalItems})
}
