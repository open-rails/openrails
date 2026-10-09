package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

type adminSubscriptionPath struct {
	SubscriptionID string `uri:"id" binding:"required"`
}

// GetAdminSubscriptions is one page of the merchant's subscriptions,
// newest first.
func GetAdminSubscriptions(r *httprequest.Request) {
	var filters subscriptions.GetSubscriptionsFilters
	if !r.BindQuery(&filters) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	ids, ok := listIDs(r, billing.ParseSubscriptionID)
	if !ok {
		return
	}
	filters.IDs = uuidutil.Of(ids)
	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "admin subscription service unavailable")
		return
	}
	subs, err := svc.ListSubscriptions(r.Request.Context(), filters, page)
	if err != nil {
		writeRefusal(r, err, "failed to list subscriptions")
		return
	}
	now := r.Clock.Now()
	r.SuccessJSON(pagination.Map(subs, func(sub *subscriptions.AdminSubscriptionResponse) billing.Subscription {
		return subscriptionView(sub, now)
	}))
}

func GetAdminSubscription(r *httprequest.Request) {
	var path adminSubscriptionPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorCode(billing.CodeInvalidParam, err.Error())
		return
	}
	typedSubscriptionID, err := billing.ParseSubscriptionID(path.SubscriptionID)
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid subscription ID")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()
	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "admin subscription service unavailable")
		return
	}
	subscription, err := svc.GetSubscriptionByID(r.Request.Context(), subscriptionID)
	if err != nil {
		writeRefusal(r, err, "failed to load subscription")
		return
	}
	r.SuccessJSON(subscriptionView(subscription, r.Clock.Now()))
}
