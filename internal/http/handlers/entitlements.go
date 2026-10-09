package handlers

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingauth"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/timeutil"
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

// ServiceListCustomerEntitlements is one page of the keys a customer holds
// at ?at= (zero: now), in byte order, optionally under ?prefix=: the keys of
// the products they hold.
func ServiceListCustomerEntitlements(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	listCustomerEntitlements(r, customer.UUID())
}

// SelfListEntitlements is one page of the customer's own keys.
func SelfListEntitlements(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	listCustomerEntitlements(r, payer.UUID())
}

func listCustomerEntitlements(r *httprequest.Request, customer uuid.UUID) {
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
	var after string
	if _, err := pagination.Decode(page.Cursor, &after); err != nil {
		writeRefusal(r, err, "invalid cursor")
		return
	}
	keys, more, err := r.State.EntitlementService.ListEntitlementsPage(r.Request.Context(), customer, r.Query("prefix"), after, limit, at)
	if err != nil {
		writeRefusal(r, err, "failed to list entitlements")
		return
	}
	out := billing.ListPage[billing.CustomerEntitlement]{Items: make([]billing.CustomerEntitlement, len(keys))}
	for i, key := range keys {
		out.Items[i] = billing.CustomerEntitlement{Entitlement: key}
	}
	if more {
		out.Next = pagination.Encode(keys[len(keys)-1])
	}
	r.SuccessJSON(out)
}

// ServiceListEntitlementCustomers is the reverse lookup: one page of the
// customers holding an active window of the path's entitlement at ?at=,
// ordered by customer id. It backs a host directory's filter by entitlement.
func ServiceListEntitlementCustomers(r *httprequest.Request) {
	entitlement := r.Param("entitlement")
	if strings.TrimSpace(entitlement) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "entitlement is required").WithParam("entitlement"))
		return
	}
	at, ok := parseAtQuery(r)
	if !ok {
		return
	}
	if at.IsZero() {
		at = r.Clock.Now()
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
	var after uuid.UUID
	if _, err := pagination.Decode(page.Cursor, &after); err != nil {
		writeRefusal(r, err, "invalid cursor")
		return
	}
	ids, err := r.State.EntitlementService.ListCustomersWithEntitlement(r.Request.Context(), entitlement, at, after, int(pagination.Fetch(limit)))
	if err != nil {
		r.InternalError("failed to list customers", err)
		return
	}
	customers := pagination.Map(pagination.Cut(ids, limit, func(id uuid.UUID) any { return id }), func(id uuid.UUID) billing.CustomerID { return billing.CustomerID(id) })
	r.SuccessJSON(customers)
}

// ServiceCheckEntitlements answers which of the requested keys the customer
// holds, and the keys they hold under each requested prefix, both at one
// instant (zero: now). It reads only those keys and key ranges.
func ServiceCheckEntitlements(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	var req billing.CheckEntitlementsParams
	if !r.BindJSON(&req) {
		return
	}
	out, err := r.State.EntitlementService.Check(r.Request.Context(), customer.String(), req)
	if err != nil {
		writeRefusal(r, err, "entitlement check failed")
		return
	}
	r.SuccessJSON(out)
}

// ServiceGetEffectiveTier answers the tier the customer holds in ?group=.
func ServiceGetEffectiveTier(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	group := strings.TrimSpace(r.Query("group"))
	if group == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "group is required").WithParam("group"))
		return
	}
	tier, err := r.State.EntitlementService.ResolveEffectiveTierByCustomer(r.Request.Context(), customer.UUID(), group, r.Clock.Now())
	if err != nil {
		r.InternalError("effective tier lookup failed", err)
		return
	}
	out := billing.EffectiveTier{Group: group}
	if tier != nil {
		out.Tier = &billing.Tier{
			Entitlement: tier.Entitlement, DisplayName: tier.ProductDisplayName, TierRank: tier.TierRank,
			ProductID: billing.ProductID(tier.ProductID), ProductKey: tier.ProductKey,
		}
	}
	r.SuccessJSON(out)
}

// maxGrantHours is the longest hours value a time.Duration holds.
const maxGrantHours = math.MaxInt64 / int64(time.Hour)

// permitPermanentGrant requires merchant:access:grant-permanent for a manual
// grant with no end, on top of the route's own permission.
func permitPermanentGrant(r *httprequest.Request, gate StaffCan) bool {
	if gate != nil {
		err := gate(r.Request, billing.MerchantAccessGrantPermanent)
		if err == nil {
			return true
		}
		if refusal := billingauth.AsRefusal(err); refusal.Status != http.StatusForbidden {
			r.AbortGate(refusal)
			return false
		}
	}
	r.ErrorCode("permanent_grant_forbidden", "a grant with no end needs "+billing.MerchantAccessGrantPermanent)
	return false
}

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
