package handlers

import (
	"errors"
	"net/http"
	"strconv"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
)

func GetMySubscriptions(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	listSubscriptionsForUser(r, user.ID)
}

func listSubscriptionsForUser(r *httprequest.Request, userID string) {
	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 10
	}

	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	status := r.Request.URL.Query().Get("status")

	queryOpts := &query.QueryOptions[services.GetSubscriptionsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: services.GetSubscriptionsFilters{},
	}

	if status != "" && status != "all" {
		queryOpts.Filters.Status = status
	}

	subscriptions, _, err := r.State.UserSubscriptionService.GetUserSubscriptionHistory(
		r.Request.Context(),
		userID,
		queryOpts,
	)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	r.SuccessJSONPaginated(subscriptions, queryOpts.TotalItems, limit, offset)
}

func GetSubscription(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	subscriptionIDStr := r.GinCtx.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorJSON(http.StatusBadRequest, "subscription ID required")
		return
	}

	subscriptionID, err := api.ParseSubscriptionID(subscriptionIDStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}

	subscription, err := r.State.UserSubscriptionService.GetUserSubscriptionByID(r.Request.Context(), user.ID, subscriptionID)
	if err != nil {
		if errors.Is(err, services.ErrSubscriptionNotFound) {
			r.ErrorJSON(http.StatusNotFound, "Subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "Failed to retrieve subscription")
		return
	}

	r.SuccessJSON(subscription)
}
