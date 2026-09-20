package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	billingservice "github.com/open-rails/openrails/internal/service"
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

	out := make([]openrails.Subscription, 0, len(subscriptions))
	for _, sub := range subscriptions {
		out = append(out, sub.View())
	}
	r.SuccessJSONPaginated(out, queryOpts.TotalItems, limit, offset)
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

	typedSubscriptionID, err := openrails.ParseSubscriptionID(subscriptionIDStr)
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()

	subscription, err := r.State.UserSubscriptionService.GetUserSubscriptionByID(r.Request.Context(), user.ID, subscriptionID)
	if err != nil {
		if errors.Is(err, subscriptions.ErrSubscriptionNotFound) {
			r.ErrorJSON(http.StatusNotFound, "Subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "Failed to retrieve subscription")
		return
	}

	out := subscription.View()
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out.Recovery, err = svc.SubscriptionRecovery(r.Request.Context(), payer, subscriptionID)
	if err != nil {
		r.InternalError("subscription recovery unavailable", err)
		return
	}
	r.SuccessJSON(out)
}
