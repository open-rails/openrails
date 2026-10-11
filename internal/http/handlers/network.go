package handlers

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/customernetwork"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/pagination"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// The network portal's reads across the account's linked merchants, and
// what it does at one of them beyond the customer routes it shares.

func networkAccount(r *httprequest.Request) (billing.NetworkAccountID, bool) {
	account, ok := customernetwork.AccountFrom(r.Request.Context())
	if !ok || r.State == nil || r.State.Network == nil {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return billing.NetworkAccountID{}, false
	}
	return account, true
}

// networkPage reads a page request and its keyset position.
func networkPage[P any](r *httprequest.Request) (int, P, bool, bool) {
	var after P
	page, ok := r.Page()
	if !ok {
		return 0, after, false, false
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		writeRefusal(r, err, "invalid page")
		return 0, after, false, false
	}
	present, err := pagination.Decode(page.Cursor, &after)
	if err != nil {
		writeRefusal(r, err, "invalid cursor")
		return 0, after, false, false
	}
	return limit, after, present, true
}

// ListNetworkLinks handles GET /v1/network/links: the account's active
// links, by merchant, each with its merchant.
func ListNetworkLinks(r *httprequest.Request) {
	account, ok := networkAccount(r)
	if !ok {
		return
	}
	type position struct {
		M uuid.UUID `json:"m"`
	}
	limit, after, present, ok := networkPage[position](r)
	if !ok {
		return
	}
	params := gen.ListActiveCustomerLinksByAccountParams{AccountID: account.UUID(), RowLimit: pagination.Fetch(limit)}
	if present {
		params.AfterMerchant = &after.M
	}
	rows, err := r.State.DB.GenDirectory().ListActiveCustomerLinksByAccount(r.Request.Context(), params)
	if err != nil {
		r.InternalError("network links failed", err)
		return
	}
	cut := pagination.Cut(rows, limit, func(row gen.BillingCustomerLink) any { return position{M: row.MerchantID} })
	out := billing.ListPage[billing.NetworkLink]{Items: make([]billing.NetworkLink, 0, len(cut.Items)), Next: cut.Next}
	for _, row := range cut.Items {
		who, ok := loadPublicMerchant(r, billing.MerchantID(row.MerchantID))
		if !ok {
			return
		}
		out.Items = append(out.Items, billing.NetworkLink{Link: customernetwork.LinkOf(row), Merchant: who})
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}

// networkPosition is a cross-merchant keyset position, newest first.
type networkPosition struct {
	At time.Time `json:"a"`
	M  uuid.UUID `json:"m"`
	I  uuid.UUID `json:"i"`
}

// networkRow is one record of a cross-merchant page.
type networkRow struct {
	link, merchant, customer, id uuid.UUID
	at                           time.Time
}

// byMerchant groups a page's records by merchant, in first-seen order.
func byMerchant(rows []networkRow) ([]uuid.UUID, map[uuid.UUID][]networkRow) {
	var order []uuid.UUID
	groups := map[uuid.UUID][]networkRow{}
	for _, row := range rows {
		if _, seen := groups[row.merchant]; !seen {
			order = append(order, row.merchant)
		}
		groups[row.merchant] = append(groups[row.merchant], row)
	}
	return order, groups
}

// ListNetworkSubscriptions handles GET /v1/network/subscriptions: the
// account's subscriptions at every linked merchant, newest first, each as
// that merchant's customer reads it.
func ListNetworkSubscriptions(r *httprequest.Request) {
	account, ok := networkAccount(r)
	if !ok {
		return
	}
	limit, after, present, ok := networkPage[networkPosition](r)
	if !ok {
		return
	}
	params := gen.ListNetworkSubscriptionPageParams{AccountID: account.UUID(), RowLimit: pagination.Fetch(limit)}
	if present {
		params.AfterAt, params.AfterMerchant, params.AfterID = &after.At, &after.M, &after.I
	}
	ctx := r.Request.Context()
	found, err := r.State.DB.GenDirectory().ListNetworkSubscriptionPage(ctx, params)
	if err != nil {
		r.InternalError("network subscriptions failed", err)
		return
	}
	rows := make([]networkRow, len(found))
	for i, f := range found {
		rows[i] = networkRow{link: f.LinkID, merchant: f.MerchantID, customer: f.CustomerID, id: f.ID, at: f.CreatedAt}
	}
	cut := pagination.Cut(rows, limit, func(row networkRow) any { return networkPosition{At: row.at, M: row.merchant, I: row.id} })
	views := map[uuid.UUID]billing.Subscription{}
	order, groups := byMerchant(cut.Items)
	for _, mid := range order {
		group := groups[mid]
		ids := make([]uuid.UUID, len(group))
		for i, row := range group {
			ids[i] = row.id
		}
		page, err := r.State.UserSubscriptionService.ListUserSubscriptions(merchant.WithID(ctx, billing.MerchantID(mid)), group[0].customer.String(),
			subscriptions.GetSubscriptionsFilters{IDs: ids}, billing.PageRequest{Limit: len(ids)})
		if err != nil {
			writeRefusal(r, err, "network subscriptions failed")
			return
		}
		for _, sub := range page.Items {
			view := sub.View()
			views[view.ID.UUID()] = view
		}
	}
	out := billing.ListPage[billing.NetworkSubscription]{Items: make([]billing.NetworkSubscription, 0, len(cut.Items)), Next: cut.Next}
	for _, row := range cut.Items {
		if view, ok := views[row.id]; ok {
			out.Items = append(out.Items, billing.NetworkSubscription{LinkID: billing.CustomerLinkID(row.link), MerchantID: billing.MerchantID(row.merchant), Subscription: view})
		}
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}

// ListNetworkOrders handles GET /v1/network/orders: the account's orders at
// every linked merchant, newest first, each as that merchant's customer
// reads it.
func ListNetworkOrders(r *httprequest.Request) {
	account, ok := networkAccount(r)
	if !ok {
		return
	}
	limit, after, present, ok := networkPage[networkPosition](r)
	if !ok {
		return
	}
	params := gen.ListNetworkOrderPageParams{AccountID: account.UUID(), RowLimit: pagination.Fetch(limit)}
	if present {
		params.AfterAt, params.AfterMerchant, params.AfterID = &after.At, &after.M, &after.I
	}
	ctx := r.Request.Context()
	found, err := r.State.DB.GenDirectory().ListNetworkOrderPage(ctx, params)
	if err != nil {
		r.InternalError("network orders failed", err)
		return
	}
	rows := make([]networkRow, len(found))
	for i, f := range found {
		rows[i] = networkRow{link: f.LinkID, merchant: f.MerchantID, customer: f.CustomerID, id: f.ID, at: f.CreatedAt}
	}
	cut := pagination.Cut(rows, limit, func(row networkRow) any { return networkPosition{At: row.at, M: row.merchant, I: row.id} })
	svc := orderService(r)
	if svc == nil {
		return
	}
	views := map[uuid.UUID]billing.Order{}
	order, groups := byMerchant(cut.Items)
	for _, mid := range order {
		group := groups[mid]
		ids := make([]billing.OrderID, len(group))
		for i, row := range group {
			ids[i] = billing.OrderID(row.id)
		}
		page, err := svc.ListOrders(merchant.WithID(ctx, billing.MerchantID(mid)), billingservice.OrderActor{Customer: billing.CustomerID(group[0].customer)},
			billing.OrderListParams{IDs: ids})
		if err != nil {
			writeOrderError(r, err)
			return
		}
		for _, o := range page.Items {
			views[o.ID.UUID()] = o
		}
	}
	out := billing.ListPage[billing.NetworkOrder]{Items: make([]billing.NetworkOrder, 0, len(cut.Items)), Next: cut.Next}
	for _, row := range cut.Items {
		if view, ok := views[row.id]; ok {
			out.Items = append(out.Items, billing.NetworkOrder{LinkID: billing.CustomerLinkID(row.link), MerchantID: billing.MerchantID(row.merchant), Order: view})
		}
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}

// UnlinkNetworkLink handles DELETE /v1/network/links/{link_id}: refused while
// the customer's subscriptions at that merchant still charge its cards.
func UnlinkNetworkLink(r *httprequest.Request) {
	account, ok := networkAccount(r)
	if !ok {
		return
	}
	id, err := billing.ParseCustomerLinkID(r.Param("link_id"))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "link_id is not a link id")
		return
	}
	if _, err := r.State.Network.Unlink(r.Request.Context(), account, id); err != nil {
		writeRefusal(r, err, "unlink failed")
		return
	}
	r.NoContent()
}

// RevokePaymentAuthority handles POST
// /v1/network/links/{link_id}/revoke-authority: the link's merchant may no
// longer charge the customer's cards without it.
func RevokePaymentAuthority(r *httprequest.Request) {
	scope, ok := r.CustomerScope()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.RevokePaymentAuthority(r.Request.Context(), scope.Customer())
	if err != nil {
		writeRefusal(r, err, "revoking payment authority failed")
		return
	}
	r.SuccessJSON(out)
}
