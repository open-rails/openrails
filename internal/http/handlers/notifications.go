package handlers

import (
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// MyNotificationsQuery is the customer's notification list query.
type MyNotificationsQuery struct {
	Seen *bool `form:"seen"`
}

// customerScope is the merchant and customer a /me request acts for.
func customerScope(r *httprequest.Request) (uuid.UUID, uuid.UUID, bool) {
	scope, ok := r.CustomerScope()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return uuid.Nil, uuid.Nil, false
	}
	return scope.Merchant().UUID(), scope.Customer().UUID(), true
}

// GetNotifications handles GET /v1/me/notifications: the customer's
// notifications, newest first.
func GetNotifications(r *httprequest.Request) {
	ctx := r.Request.Context()
	var query MyNotificationsQuery
	if !r.BindQuery(&query) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	merchantID, customerID, ok := customerScope(r)
	if !ok {
		return
	}
	notes, err := customerNotificationPage(page, func(afterAt *time.Time, afterID *uuid.UUID, fetch int32) ([]gen.BillingNotification, error) {
		return r.State.DB.Gen(ctx).ListCustomerNotifications(ctx, gen.ListCustomerNotificationsParams{
			MerchantID: merchantID, CustomerID: customerID, Seen: query.Seen, AfterAt: afterAt, AfterID: afterID, RowLimit: fetch,
		})
	})
	if err != nil {
		writeRefusal(r, err, "list notifications failed")
		return
	}
	r.SuccessJSON(notes)
}

// MarkMyNotificationsRead handles POST /v1/me/notifications/read: 1 to
// billing.MaxBatchItems of the customer's notifications marked read. Another
// customer's id is null and untouched.
func MarkMyNotificationsRead(r *httprequest.Request) {
	ctx := r.Request.Context()
	var req billing.MarkNotificationsReadParams
	if !r.BindJSON(&req) {
		return
	}
	if req.All {
		if len(req.NotificationIDs) > 0 {
			r.APIError(api.Coded(billing.CodeInvalidParam, "all takes no notification_ids").WithParam("all"))
			return
		}
		merchantID, customerID, ok := customerScope(r)
		if !ok {
			return
		}
		if _, err := r.State.DB.Gen(ctx).MarkAllCustomerNotificationsRead(ctx, gen.MarkAllCustomerNotificationsReadParams{MerchantID: merchantID, CustomerID: customerID}); err != nil {
			r.InternalError("mark notifications read failed", err)
			return
		}
		r.SuccessJSON(billing.CustomerNotificationLookup{Notifications: map[billing.NotificationID]*billing.Notification{}})
		return
	}
	ids, ok := batchIDs(r, req.NotificationIDs, billing.MaxBatchItems, "notification_ids")
	if !ok {
		return
	}
	merchantID, customerID, ok := customerScope(r)
	if !ok {
		return
	}
	rows, err := r.State.DB.Gen(ctx).MarkCustomerNotificationsRead(ctx, gen.MarkCustomerNotificationsReadParams{MerchantID: merchantID, CustomerID: customerID, Ids: uuidutil.Of(ids)})
	if err != nil {
		r.InternalError("mark notifications read failed", err)
		return
	}
	out := billing.CustomerNotificationLookup{Notifications: make(map[billing.NotificationID]*billing.Notification, len(ids))}
	for _, id := range ids {
		out.Notifications[id] = nil
	}
	for _, row := range rows {
		n, err := models.NotificationFromGen(row)
		if err != nil {
			r.InternalError("decode notification failed", err)
			return
		}
		view := n.View()
		out.Notifications[view.ID] = &view
	}
	r.SuccessJSON(out)
}

// customerNotificationPage runs a keyset notification query for one page.
func customerNotificationPage(page billing.PageRequest, fetch func(*time.Time, *uuid.UUID, int32) ([]gen.BillingNotification, error)) (billing.ListPage[billing.Notification], error) {
	limit, err := pagination.Limit(page)
	if err != nil {
		return billing.ListPage[billing.Notification]{}, err
	}
	afterAt, afterID, err := pagination.After(page.Cursor)
	if err != nil {
		return billing.ListPage[billing.Notification]{}, err
	}
	rows, err := fetch(afterAt, afterID, pagination.Fetch(limit))
	if err != nil {
		return billing.ListPage[billing.Notification]{}, err
	}
	cut := pagination.Cut(rows, limit, func(n gen.BillingNotification) any { return pagination.TimeID{At: n.CreatedAt, ID: n.ID} })
	out := billing.ListPage[billing.Notification]{Next: cut.Next, Items: make([]billing.Notification, 0, len(cut.Items))}
	for _, row := range cut.Items {
		n, err := models.NotificationFromGen(row)
		if err != nil {
			return billing.ListPage[billing.Notification]{}, err
		}
		out.Items = append(out.Items, n.View())
	}
	return out, nil
}

// #528: GetAdminProviderIntents (the #358 provider-intent ledger debug view) was
// dropped — it lived only on the retired per-user admin surface.
// #666: GetAdminManualRebillAttempts (never routed) was dropped with it.
