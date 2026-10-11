package handlers

import (
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/pagination"
)

// ListNetworkNotifications reads the account's customer notifications through
// its current active links. Operator findings and unlinked customers cannot
// enter this query. The link-scoped /notifications/read route shares /me state.
func ListNetworkNotifications(r *httprequest.Request) {
	account, ok := networkAccount(r)
	if !ok {
		return
	}
	var query MyNotificationsQuery
	if !r.BindQuery(&query) {
		return
	}
	limit, after, present, ok := networkPage[networkPosition](r)
	if !ok {
		return
	}
	params := gen.ListNetworkNotificationPageParams{AccountID: account.UUID(), Seen: query.Seen, RowLimit: pagination.Fetch(limit)}
	if present {
		params.AfterAt, params.AfterMerchant, params.AfterID = &after.At, &after.M, &after.I
	}
	rows, err := r.State.DB.GenDirectory().ListNetworkNotificationPage(r.Request.Context(), params)
	if err != nil {
		r.InternalError("network notifications failed", err)
		return
	}
	cut := pagination.Cut(rows, limit, func(row gen.ListNetworkNotificationPageRow) any {
		return networkPosition{At: row.CreatedAt, M: row.MerchantID, I: row.ID}
	})
	out := billing.ListPage[billing.NetworkNotification]{Items: make([]billing.NetworkNotification, 0, len(cut.Items)), Next: cut.Next}
	merchants := make(map[uuid.UUID]billing.PublicMerchant)
	for _, row := range cut.Items {
		notice, err := models.NotificationFromGen(gen.BillingNotification{
			ID: row.ID, MerchantID: row.MerchantID, CustomerID: row.CustomerID, RecipientKind: "customer",
			EventType: row.EventType, Data: row.Data, ReadAt: row.ReadAt, CreatedAt: row.CreatedAt,
		})
		if err != nil {
			r.InternalError("decode notification failed", err)
			return
		}
		who, loaded := merchants[row.MerchantID]
		if !loaded {
			var ok bool
			who, ok = loadPublicMerchant(r, billing.MerchantID(row.MerchantID))
			if !ok {
				return
			}
			merchants[row.MerchantID] = who
		}
		out.Items = append(out.Items, billing.NetworkNotification{LinkID: billing.CustomerLinkID(row.LinkID),
			MerchantID: billing.MerchantID(row.MerchantID), Merchant: who, Notification: notice.View()})
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}
