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
	"github.com/open-rails/openrails/internal/merchant"
)

// MyNotificationsQuery is the customer's notification list query.
type MyNotificationsQuery struct {
	Seen *bool `form:"seen"`
}

// customerScope is the merchant and customer a /me request acts for.
func customerScope(r *httprequest.Request) (uuid.UUID, uuid.UUID, bool) {
	merchantID, err := merchant.Require(r.Request.Context())
	if err != nil {
		writeRefusal(r, err, "merchant scope required")
		return uuid.Nil, uuid.Nil, false
	}
	customer, err := billing.ParseCustomerID(r.GetUser().ID)
	if err != nil || customer.IsZero() {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return uuid.Nil, uuid.Nil, false
	}
	return merchantID.UUID(), customer.UUID(), true
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
