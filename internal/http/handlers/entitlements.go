package handlers

import (
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ListEntitlements is one page of the keys customers hold at ?at= (zero:
// now), by customer then key: of the customers ?customer_id= names (comma
// separated), only ?entitlement= keys (repeated) and keys under ?prefix=.
// Without customer_id, the one ?entitlement= key's holders.
func ListEntitlements(r *httprequest.Request) {
	var params billing.EntitlementListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	customers, ok := uniqueIDs(r, "customer_id", params.CustomerIDs)
	if !ok {
		return
	}
	for _, id := range customers {
		if !requireServiceCustomerScope(r, id) {
			return
		}
	}
	if len(customers) == 0 && !requireMerchantRoutePrincipal(r) {
		return
	}
	params.CustomerIDs = customers
	listEntitlements(r, params)
}

// MyEntitlementListQuery is a customer's own keys' query.
type MyEntitlementListQuery struct {
	billing.PageRequest
	Prefix string    `form:"prefix"`
	At     time.Time `form:"at"`
}

// SelfListEntitlements is one page of the customer's own keys.
func SelfListEntitlements(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	var query MyEntitlementListQuery
	if !bindList(r, &query, &query.PageRequest) {
		return
	}
	listEntitlements(r, billing.EntitlementListParams{PageRequest: query.PageRequest, CustomerIDs: []billing.CustomerID{billing.CustomerID(payer)}, Prefix: query.Prefix, At: query.At})
}

// entitlementCursor is the last row of a page of held keys.
type entitlementCursor struct {
	Customer uuid.UUID `json:"c"`
	Key      string    `json:"k"`
}

func listEntitlements(r *httprequest.Request, params billing.EntitlementListParams) {
	page := params.PageRequest
	limit, err := pagination.Limit(page)
	if err != nil {
		writeRefusal(r, err, "invalid page")
		return
	}
	var after entitlementCursor
	if _, err := pagination.Decode(page.Cursor, &after); err != nil {
		writeRefusal(r, err, "invalid cursor")
		return
	}
	rows, more, err := r.State.EntitlementService.List(r.Request.Context(), entitlements.Query{
		Customers: uuidutil.Of(params.CustomerIDs), Keys: params.Entitlements, Prefix: params.Prefix, At: params.At.UTC(),
		After: entitlements.Held{Customer: after.Customer, Key: after.Key}, Limit: limit,
	})
	if err != nil {
		writeRefusal(r, err, "failed to list entitlements")
		return
	}
	out := billing.ListPage[billing.CustomerEntitlement]{Items: make([]billing.CustomerEntitlement, len(rows))}
	for i, row := range rows {
		out.Items[i] = billing.CustomerEntitlement{CustomerID: billing.CustomerID(row.Customer), Entitlement: row.Key, Quantity: row.Seats}
	}
	if more {
		last := rows[len(rows)-1]
		out.Next = pagination.Encode(entitlementCursor{Customer: last.Customer, Key: last.Key})
	}
	r.SuccessJSON(out)
}

// AppCheckEntitlements answers an application's content gate: which of the
// requested keys the body's customer holds, and the keys they hold under each
// requested prefix, at one instant (zero: now). A current exact-key check with
// include_validity also answers evaluated_at and valid_until.
func AppCheckEntitlements(r *httprequest.Request) {
	var req billing.CheckEntitlementsParams
	if !r.BindJSON(&req) {
		return
	}
	customer, ok := commerceCustomer(r, req.CustomerID)
	if !ok {
		return
	}
	out, err := r.State.EntitlementService.Check(r.Request.Context(), customer.UUID(), req)
	if err != nil {
		writeRefusal(r, err, "entitlement check failed")
		return
	}
	r.SuccessJSON(out)
}

// maxGrantHours is the longest hours value a time.Duration holds.
const maxGrantHours = math.MaxInt64 / int64(time.Hour)
