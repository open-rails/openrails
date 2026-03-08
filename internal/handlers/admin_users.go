package handlers

import (
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
)

// AdminUserPath is the path parameter for admin user endpoints
type AdminUserPath struct {
	UserID string `uri:"user_id" binding:"required"`
}

// AdminUserBillingProfile contains a user's complete billing information
type AdminUserBillingProfile struct {
	UserID       string               `json:"user_id"`
	Subscription *models.Subscription `json:"subscription,omitempty"`
	Entitlements []models.Entitlement `json:"entitlements"`
	Payments     []*models.Payment    `json:"payments"`
}

// GetAdminUserBillingProfile returns a user's complete billing profile
// GET /v1/admin/users/:user_id
func GetAdminUserBillingProfile(r *httprequest.Request) {
	var path AdminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Request.Context()
	now := time.Now()

	profile := AdminUserBillingProfile{
		UserID:       path.UserID,
		Entitlements: []models.Entitlement{},
		Payments:     []*models.Payment{},
	}

	// Get active subscription
	if r.State.SubscriptionService != nil {
		sub, err := r.State.SubscriptionService.GetActiveSubscription(ctx, path.UserID)
		if err == nil && sub != nil {
			profile.Subscription = sub
		}
		// If no active subscription, that's fine - user may have only one-off payments
	}

	// Get active entitlements
	if r.State.EntitlementService != nil {
		ents, err := r.State.EntitlementService.ListActiveRecords(ctx, path.UserID, now)
		if err == nil && len(ents) > 0 {
			profile.Entitlements = ents
		}
	}

	// Get payments (use the payment service with filters for user)
	if r.State.PaymentService != nil {
		payments, err := r.State.PaymentService.GetByUserID(ctx, path.UserID)
		if err == nil && len(payments) > 0 {
			profile.Payments = payments
		}
	}

	r.SuccessJSON(profile)
}

// GetAdminSubscriptions returns a paginated list of subscriptions with filters
// GET /v1/admin/subscriptions
func GetAdminSubscriptions(r *httprequest.Request) {
	queryOpts := query.QueryOptions[services.GetSubscriptionsFilters]{
		Limit:  50,
		Offset: 0,
	}

	// Parse query parameters
	if err := r.Inner().ShouldBindQuery(&queryOpts); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	// Parse filters from query params
	var filters services.GetSubscriptionsFilters
	if err := r.Inner().ShouldBindQuery(&filters); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	queryOpts.Filters = filters

	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "admin subscription service unavailable")
		return
	}

	subscriptions, total, err := svc.GetAllSubscriptions(r.Request.Context(), &queryOpts)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	r.SuccessJSONPaginated(subscriptions, total, queryOpts.Limit, queryOpts.Offset)
}

// AdminSubscriptionPath is the path parameter for admin subscription endpoints
type AdminSubscriptionPath struct {
	SubscriptionID string `uri:"id" binding:"required"`
}

// GetAdminSubscription returns a single subscription with full details including payments
// GET /v1/admin/subscriptions/:id
func GetAdminSubscription(r *httprequest.Request) {
	var path AdminSubscriptionPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	subscriptionID, err := api.ParseSubscriptionID(path.SubscriptionID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription ID")
		return
	}

	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "admin subscription service unavailable")
		return
	}

	subscription, err := svc.GetSubscriptionByID(r.Request.Context(), subscriptionID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, err.Error())
		return
	}

	r.SuccessJSON(subscription)
}
