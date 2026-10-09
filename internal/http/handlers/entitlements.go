package handlers

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// convergeAfterMutation runs the inline Convergence Engine for one customer after
// a successful state mutation (#511): the customer is left consistent by
// construction. Best-effort — a convergence failure is logged, never surfaced to
// the caller (the mutation already succeeded; the background sweep is the
// backstop). Cheap: a customer-scoped Converge scans only that customer's rows and
// writes nothing when already consistent.
func convergeAfterMutation(r *httprequest.Request, customer uuid.UUID) {
	mID, ok := merchant.FromContext(r.Request.Context())
	if !ok || customer == uuid.Nil {
		return
	}
	if _, err := converge.AfterMutation(r.Request.Context(), r.State.DB, mID, customer, r.Clock); err != nil {
		log.WithContext(r.Request.Context()).WithError(err).
			Warn("inline converge after mutation failed; background sweep will reconcile")
	}
}

// ListEntitlements is one page of the keys customers hold at ?at= (zero:
// now), by customer then key: of the customers ?customer_id= names (comma
// separated), only ?entitlement= keys (repeated) and keys under ?prefix=.
// Without customer_id, the one ?entitlement= key's holders.
func ListEntitlements(r *httprequest.Request) {
	customers, ok := queryCustomerIDs(r, "customer_id")
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
	listEntitlements(r, customers, r.Request.URL.Query()["entitlement"])
}

// SelfListEntitlements is one page of the customer's own keys.
func SelfListEntitlements(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	listEntitlements(r, []billing.CustomerID{billing.CustomerID(payer)}, nil)
}

// entitlementCursor is the last row of a page of held keys.
type entitlementCursor struct {
	Customer uuid.UUID `json:"c"`
	Key      string    `json:"k"`
}

func listEntitlements(r *httprequest.Request, customers []billing.CustomerID, keys []string) {
	at, ok := parseAtQuery(r)
	if !ok {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
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
		Customers: uuidutil.Of(customers), Keys: keys, Prefix: r.Query("prefix"), At: at,
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

// queryCustomerIDs reads a comma-separated customer id filter: 1 to
// billing.MaxBatchItems distinct ids, or none when the request names none.
func queryCustomerIDs(r *httprequest.Request, name string) ([]billing.CustomerID, bool) {
	raw := strings.TrimSpace(r.Query(name))
	if raw == "" {
		return nil, true
	}
	parts := strings.Split(raw, ",")
	if len(parts) > billing.MaxBatchItems {
		r.APIError(api.Coded(billing.CodeInvalidQuery, fmt.Sprintf("%s names at most %d customers", name, billing.MaxBatchItems)).WithParam(name))
		return nil, false
	}
	out := make([]billing.CustomerID, 0, len(parts))
	seen := make(map[billing.CustomerID]bool, len(parts))
	for _, part := range parts {
		id, err := billing.ParseCustomerID(strings.TrimSpace(part))
		if err != nil || id.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidQuery, fmt.Sprintf("%s holds an invalid id %q", name, part)).WithParam(name))
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, true
}

// AppCheckEntitlements answers an application's content gate: which of the
// requested keys the body's customer holds, and the keys they hold under each
// requested prefix, at one instant (zero: now).
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

// parseAtQuery reads an optional RFC3339 `at` query param. When absent, returns a
// zero time (defaulted to now by the caller).
func parseAtQuery(r *httprequest.Request) (time.Time, bool) {
	atStr := strings.TrimSpace(r.Query("at"))
	if atStr == "" {
		return time.Time{}, true
	}
	parsed, err := timeutil.ParseRFC3339UTC(atStr)
	if err != nil {
		r.ErrorCode(billing.CodeInvalidParam, "invalid 'at' timestamp format; use RFC3339")
		return time.Time{}, false
	}
	return parsed, true
}
