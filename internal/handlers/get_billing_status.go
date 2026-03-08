package handlers

import (
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
)

type BillingStatusResponse struct {
	HasActiveSubscription bool                               `json:"has_active_subscription"`
	Subscription          *services.UserSubscriptionResponse `json:"subscription,omitempty"`
	NextRenewalAt         *time.Time                         `json:"next_renewal_at,omitempty"`
	Entitlements          []models.Entitlement               `json:"entitlements,omitempty"`
}

func GetMyBillingStatus(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorJSON(http.StatusUnauthorized, "unauthorized")
		return
	}

	// Subscription details
	var sub *services.UserSubscriptionResponse
	var next *time.Time
	var hasActive bool
	if r.State.UserSubscriptionService != nil {
		resp, err := r.State.UserSubscriptionService.GetUserSubscription(r.Request.Context(), user.ID)
		if err == nil && resp != nil {
			sub = resp
			if resp.Subscription != nil {
				// Check if subscription is active
				hasActive = resp.Subscription.Status == models.StatusActive
				if resp.Subscription.CurrentPeriodEndsAt != nil {
					next = resp.Subscription.CurrentPeriodEndsAt
				}
			}
		}
	}

	// List entitlements (optional)
	var ents []models.Entitlement
	if r.State.EntitlementService != nil {
		list, err := r.State.EntitlementService.ListByUser(r.Request.Context(), user.ID)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, err.Error())
			return
		}
		ents = list
	}

	r.SuccessJSON(BillingStatusResponse{
		HasActiveSubscription: hasActive,
		Subscription:          sub,
		NextRenewalAt:         next,
		Entitlements:          ents,
	})
}
