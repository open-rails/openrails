package handlers

import (
	"errors"
	"net/http"
	"strconv"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
	billingservice "github.com/open-rails/openrails/pkg/service"
	log "github.com/sirupsen/logrus"
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

	queryOpts := &query.QueryOptions[subscriptions.GetSubscriptionsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: subscriptions.GetSubscriptionsFilters{},
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
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve subscriptions")
		return
	}

	for _, sub := range subscriptions {
		if !attachSubscriptionRecovery(r, sub) {
			return
		}
	}
	r.SuccessJSONPaginated(subscriptions, queryOpts.TotalItems, limit, offset)
}

// attachSubscriptionRecovery fills the customer's retry-now state (#809) on
// a self-route view, or writes the error and returns false.
func attachSubscriptionRecovery(r *httprequest.Request, resp *subscriptions.UserSubscriptionResponse) bool {
	if resp == nil || resp.Subscription == nil {
		return true
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return false
	}
	recovery, err := svc.SubscriptionRecovery(r.Request.Context(), resp.Subscription)
	if err != nil {
		log.WithContext(r.Request.Context()).WithError(err).WithField("subscription_id", resp.Subscription.ID).Error("subscription recovery state failed")
		r.ErrorJSON(http.StatusInternalServerError, "failed to derive subscription recovery state")
		return false
	}
	resp.Recovery = recovery
	return true
}

func GetSubscription(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	subscriptionIDStr := r.Param("id")
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
		if errors.Is(err, subscriptions.ErrSubscriptionNotFound) {
			r.ErrorJSON(http.StatusNotFound, "Subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "Failed to retrieve subscription")
		return
	}

	if !attachSubscriptionRecovery(r, subscription) {
		return
	}
	r.SuccessJSON(subscription)
}
