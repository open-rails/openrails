package handlers

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// customerContacts are the directory contacts of a staff list's customers,
// in one lookup. A directory that cannot answer leaves them out: the list
// still loads, each row naming its customer by id.
func customerContacts(r *httprequest.Request, customers []billing.CustomerID) map[billing.CustomerID]*billing.CustomerContact {
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if r.State.Contacts == nil || len(customers) == 0 || err != nil {
		return nil
	}
	seen := map[billing.CustomerID]bool{}
	var ids []uuid.UUID
	for _, id := range customers {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id.UUID())
		}
	}
	found, err := r.State.Contacts.Contacts(ctx, mid, ids)
	if err != nil {
		r.Log.WarnContext(ctx, "list customer contacts unavailable", logx.Err(err))
		return nil
	}
	out := make(map[billing.CustomerID]*billing.CustomerContact, len(found))
	for id, contact := range found {
		out[billing.CustomerID(id)] = contact.Customer()
	}
	return out
}

// instant is t, nil when zero.
func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

type adminSubscriptionPath struct {
	SubscriptionID string `uri:"id" binding:"required"`
}

// GetAdminSubscriptions is one page of the merchant's subscriptions,
// newest first.
func GetAdminSubscriptions(r *httprequest.Request) {
	var params billing.SubscriptionListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	ids, ok := listIDs(r, params.IDs)
	if !ok {
		return
	}
	page := params.PageRequest
	filters := subscriptions.GetSubscriptionsFilters{
		CustomerID: params.CustomerID, Status: string(params.Status), PriceID: params.PriceID, Rail: params.Rail,
		CreatedAfter: instant(params.CreatedAfter), CreatedBefore: instant(params.CreatedBefore), CanceledAfter: instant(params.CanceledAfter),
		CanceledBefore: instant(params.CanceledBefore), ExpiresBefore: instant(params.ExpiresBefore), Dunning: params.Dunning, Sort: params.Sort,
		IDs: uuidutil.Of(ids),
	}
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
	out := pagination.Map(subs, func(sub *subscriptions.AdminSubscriptionResponse) billing.Subscription {
		return subscriptionView(sub, now)
	})
	customers := make([]billing.CustomerID, len(out.Items))
	for i, sub := range out.Items {
		customers[i] = sub.CustomerID
	}
	contacts := customerContacts(r, customers)
	for i := range out.Items {
		out.Items[i].CustomerContact = contacts[out.Items[i].CustomerID]
	}
	writeSubscriptions(r, out)
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
	writeSubscription(r, subscriptionView(subscription, r.Clock.Now()))
}
