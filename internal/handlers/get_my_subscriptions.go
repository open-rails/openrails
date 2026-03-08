package handlers

import (
	"net/http"
	"strconv"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/query"
)

// GetMySubscriptions retrieves the user's subscriptions with query param filtering
// Query params:
//   - status: filter by status (active, cancelled, past_due, all). Default: non-cancelled
//   - limit: max results (1-100, default 10)
//   - offset: pagination offset (default 0)
func GetMySubscriptions(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	listSubscriptionsForUser(r, user.ID)
}

// listSubscriptionsForUser handles the core listing logic and is reused by customer-scoped routes.
func listSubscriptionsForUser(r *httprequest.Request, userID string) {
	// Parse query parameters
	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 10
	}

	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	status := r.Request.URL.Query().Get("status")

	// Build query options
	queryOpts := &query.QueryOptions[services.GetSubscriptionsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: services.GetSubscriptionsFilters{},
	}

	// Handle status filtering
	// Default behavior: show non-cancelled subscriptions (like Stripe)
	// status=all: show everything including cancelled
	// status=active: only active
	// status=cancelled: only cancelled
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
