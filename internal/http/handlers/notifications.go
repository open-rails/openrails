package handlers

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/pagination"
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

// MarkNotificationRead handles POST /v1/me/notifications/{id}/read.
func MarkNotificationRead(r *httprequest.Request) {
	ctx := r.Request.Context()
	id, ok := pathID(r, billing.ParseNotificationID)
	if !ok {
		return
	}
	merchantID, customerID, ok := customerScope(r)
	if !ok {
		return
	}
	row, err := r.State.DB.Gen(ctx).MarkCustomerNotificationRead(ctx, gen.MarkCustomerNotificationReadParams{MerchantID: merchantID, CustomerID: customerID, ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorCode(billing.CodeResourceNotFound, "")
		return
	}
	if err != nil {
		r.InternalError("mark notification read failed", err)
		return
	}
	n, err := models.NotificationFromGen(row)
	if err != nil {
		r.InternalError("decode notification failed", err)
		return
	}
	r.SuccessJSON(n.View())
}

// GetUnreadNotificationCount handles GET /v1/me/notifications/unread-count.
func GetUnreadNotificationCount(r *httprequest.Request) {
	ctx := r.Request.Context()
	merchantID, customerID, ok := customerScope(r)
	if !ok {
		return
	}
	count, err := r.State.DB.Gen(ctx).CountUnreadCustomerNotifications(ctx, gen.CountUnreadCustomerNotificationsParams{MerchantID: merchantID, CustomerID: customerID})
	if err != nil {
		r.InternalError("count unread notifications failed", err)
		return
	}
	r.SuccessJSON(billing.UnreadCount{UnreadCount: count})
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
