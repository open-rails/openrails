package handlers

import (
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
)

// RepairAlertsQuery is the repair alert list's query.
type RepairAlertsQuery struct {
	Seen *bool `form:"seen"`
}

// GetAdminRepairAlerts handles GET /v1/merchant/repair-alerts: ledger repairs
// that need the merchant, newest first.
func GetAdminRepairAlerts(r *httprequest.Request) {
	ctx := r.Request.Context()
	var query RepairAlertsQuery
	if !r.BindQuery(&query) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		writeRefusal(r, err, "merchant scope required")
		return
	}
	notes, err := customerNotificationPage(page, func(afterAt *time.Time, afterID *uuid.UUID, fetch int32) ([]gen.BillingNotification, error) {
		return r.State.DB.Gen(ctx).ListRepairAlerts(ctx, gen.ListRepairAlertsParams{
			MerchantID: merchantID.UUID(), CustomerID: db.SystemCustomerID(merchantID.UUID()), EventType: string(models.NotificationSystemAlert),
			Seen: query.Seen, AfterAt: afterAt, AfterID: afterID, RowLimit: fetch,
		})
	})
	if err != nil {
		writeRefusal(r, err, "list repair alerts failed")
		return
	}
	r.SuccessJSON(notes)
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
