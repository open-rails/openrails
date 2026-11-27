package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/pagination"
)

// MySubscriptionListQuery filters the customer's own subscriptions; status
// "all" (or none) lists every status.
type MySubscriptionListQuery struct {
	billing.PageRequest
	Status string `form:"status"`
}

// GetMySubscriptions is one page of the customer's own subscriptions,
// newest first.
func GetMySubscriptions(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "User authentication required")
		return
	}
	var q MySubscriptionListQuery
	if !bindList(r, &q, &q.PageRequest) {
		return
	}
	page := q.PageRequest
	var filters subscriptions.GetSubscriptionsFilters
	if q.Status != "all" {
		filters.Status = q.Status
	}
	subs, err := r.State.CustomerSubscriptionService.ListCustomerSubscriptions(r.Request.Context(), user.ID, filters, page)
	if err != nil {
		writeRefusal(r, err, "failed to retrieve subscriptions")
		return
	}
	writeSubscriptions(r, pagination.Map(subs, func(sub *subscriptions.CustomerSubscriptionResponse) billing.Subscription { return sub.View() }))
}

// GetSubscription reads one of the customer's own subscriptions, with its
// recovery state.
func GetSubscription(r *httprequest.Request) {
	userID, sub, ok := ownSubscription(r)
	if !ok {
		return
	}
	writeMySubscription(r, userID, sub.ID, nil)
}
