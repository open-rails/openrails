package handlers

import (
	"net/http"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

func GetMyBillingStatus(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorJSON(http.StatusUnauthorized, "unauthorized")
		return
	}

	out := openrails.BillingStatus{}
	if r.State.UserSubscriptionService != nil {
		resp, err := r.State.UserSubscriptionService.GetUserSubscription(r.Request.Context(), user.ID)
		if err == nil {
			out.Access = resp.Access
			if resp.Subscription != nil {
				view := resp.View()
				if !attachSubscriptionRecovery(r, resp, &view) {
					return
				}
				out.Subscription = &view
				out.HasActiveSubscription = resp.Subscription.Status == models.StatusActive
				out.NextRenewalAt = resp.Subscription.CurrentPeriodEndsAt
			}
		}
	}

	if r.State.EntitlementService != nil {
		list, err := r.State.EntitlementService.ListByUser(r.Request.Context(), user.ID)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve entitlements")
			return
		}
		for i := range list {
			out.Entitlements = append(out.Entitlements, entitlementRecordFromModel(&list[i]))
		}
	}

	r.SuccessJSON(out)
}
