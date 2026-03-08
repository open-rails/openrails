package handlers

import (
	"errors"
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
)

// GetSubscription retrieves a single subscription by ID
// GET /v1/me/subscriptions/:id
func GetSubscription(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	// Parse subscription ID from path
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
