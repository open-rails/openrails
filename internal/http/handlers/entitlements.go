package handlers

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingauth"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/reconcile/converge"
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

// ServiceListEntitlements answers the active entitlements of up to
// billing.MaxEntitlementLookupCustomers customers in one read: every requested
// customer is present, one with none maps to an empty list.
func ServiceListEntitlements(r *httprequest.Request) {
	var req billing.EntitlementListParams
	if !r.BindJSON(&req) {
		return
	}
	ids := make([]uuid.UUID, 0, len(req.CustomerIDs))
	seen := make(map[billing.CustomerID]bool, len(req.CustomerIDs))
	for _, id := range req.CustomerIDs {
		if id.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidParam, "customer_ids must be nonzero UUIDs").WithParam("customer_ids"))
			return
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id.UUID())
		}
	}
	switch {
	case len(ids) == 0:
		r.APIError(api.Coded(billing.CodeInvalidParam, "customer_ids is required").WithParam("customer_ids"))
		return
	case len(ids) > billing.MaxEntitlementLookupCustomers:
		r.APIError(api.Coded(billing.CodeInvalidParam, fmt.Sprintf("at most %d customer_ids per call", billing.MaxEntitlementLookupCustomers)).WithParam("customer_ids"))
		return
	}
	for _, id := range ids {
		if !requireServiceCustomerScope(r, identity.CustomerID(id)) {
			return
		}
	}
	at := req.At
	if at.IsZero() {
		at = r.Clock.Now()
	}
	grouped, err := r.State.EntitlementService.ListActiveRecordsByCustomers(r.Request.Context(), ids, at)
	if err != nil {
		r.InternalError("failed to fetch entitlements", err)
		return
	}
	out := billing.EntitlementLookup{Customers: make(map[billing.CustomerID][]billing.EntitlementRecord, len(ids))}
	for _, id := range ids {
		records := make([]billing.EntitlementRecord, 0, len(grouped[id]))
		for i := range grouped[id] {
			records = append(records, entitlementRecord(&grouped[id][i]))
		}
		out.Customers[billing.CustomerID(id)] = records
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
// holds at at (zero: now). It reads only those keys.
func ServiceCheckEntitlements(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	var req billing.CheckEntitlementsParams
	if !r.BindJSON(&req) {
		return
	}
	at := req.At
	if at.IsZero() {
		at = r.Clock.Now()
	}
	result, err := r.State.EntitlementService.CheckMany(r.Request.Context(), customer.String(), req.Entitlements, at)
	if err != nil {
		writeRefusal(r, err, "entitlement check failed")
		return
	}
	r.SuccessJSON(billing.EntitlementCheck{Entitlements: result})
}

// SelfListEntitlements is the customer's own active entitlements.
func SelfListEntitlements(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	at, ok := parseAtQuery(r)
	if !ok {
		return
	}
	if at.IsZero() {
		at = r.Clock.Now()
	}
	windows, err := r.State.EntitlementService.ListActiveRecordsByCustomer(r.Request.Context(), payer.UUID(), at)
	if err != nil {
		r.InternalError("failed to resolve active entitlements", err)
		return
	}
	records := make([]billing.EntitlementRecord, 0, len(windows))
	for i := range windows {
		records = append(records, entitlementRecord(&windows[i]))
	}
	r.SuccessJSON(billing.ListPage[billing.EntitlementRecord]{Items: records})
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

// CreateEntitlement records the merchant's own grant of an entitlement. One
// with no end also needs merchant:access:grant-permanent.
func CreateEntitlement(gate billingauth.Gate) func(*httprequest.Request) {
	return func(r *httprequest.Request) { createEntitlement(r, gate) }
}

func createEntitlement(r *httprequest.Request, gate billingauth.Gate) {
	customerID := customerIDParam(r.Param("customer_id"))
	if customerID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid customer_id").WithParam("customer_id"))
		return
	}
	var req billing.CreateEntitlementParams
	if !r.BindJSON(&req) {
		return
	}
	req.Entitlement = strings.TrimSpace(req.Entitlement)
	if req.Entitlement == "" {
		r.ErrorCode(billing.CodeInvalidParam, "entitlement is required")
		return
	}
	svc := r.State.EntitlementService
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "entitlement service unavailable")
		return
	}
	// #511: a manual grant is an admin-sourced ledger fact whose SourceID is
	// the grant's own identity. Hours extends the finite timeline; EndsAt fixes
	// this grant's own end; neither is indefinite.
	params := entitlements.PushNewEntitlementParams{UserID: customerID.String(), Entitlement: req.Entitlement, SourceType: models.EntitlementSourceAdmin, SourceID: uuidutil.NewV7()}
	switch {
	case req.Hours != nil && req.EndsAt != nil:
		r.ErrorCode(billing.CodeInvalidParam, "hours and ends_at are mutually exclusive")
		return
	case req.Hours != nil:
		if *req.Hours <= 0 || int64(*req.Hours) > maxGrantHours {
			r.ErrorCode(billing.CodeInvalidParam, fmt.Sprintf("hours must be between 1 and %d (or omit for indefinite)", maxGrantHours))
			return
		}
		d := time.Duration(*req.Hours) * time.Hour
		params.Duration = &d
	case req.EndsAt != nil:
		if !req.EndsAt.After(r.Clock.Now()) {
			r.ErrorCode(billing.CodeInvalidParam, "ends_at must be in the future")
			return
		}
		endAt := req.EndsAt.UTC()
		params.EndsAt = &endAt
	default:
		if !permitPermanentGrant(r, gate) {
			return
		}
		params.Indefinite = true
	}
	var err error
	params.CustomerID, err = tenantSubjectForEntitlementGrantTarget(r, customerID.String())
	if err != nil {
		r.ErrorCode(billing.CodeInternalError, "failed to resolve target tenant subject")
		return
	}
	ent, err := svc.PushNewEntitlement(r.Request.Context(), params)
	if err != nil {
		r.ErrorCode(billing.CodeInternalError, err.Error())
		return
	}
	convergeAfterMutation(r, params.CustomerID) // #511: re-converge the customer inline
	r.JSON(http.StatusCreated, entitlementRecord(ent))
}

// permitPermanentGrant requires merchant:access:grant-permanent for a manual
// grant with no end, on top of the route's own permission.
func permitPermanentGrant(r *httprequest.Request, gate billingauth.Gate) bool {
	if gate != nil {
		_, err := gate.Authorize(r.Request.Context(), r.Request, billing.MerchantAccessGrantPermanent)
		if err == nil {
			return true
		}
		var refusal billingauth.GateError
		if errors.As(err, &refusal) && refusal.Status != http.StatusForbidden {
			r.AbortGate(refusal)
			return false
		}
	}
	r.ErrorCode("permanent_grant_forbidden", "a grant with no end needs "+billing.MerchantAccessGrantPermanent)
	return false
}

// entitlementRecord is the one projection of an entitlement window onto the
// wire.
func entitlementRecord(e *models.Entitlement) billing.EntitlementRecord {
	rec := billing.EntitlementRecord{
		ID: billing.EntitlementID(e.ID), CustomerID: billing.CustomerID(e.CustomerID), Entitlement: e.Entitlement,
		StartsAt: e.StartsAt, EndsAt: e.EndsAt, SourceType: billing.EntitlementSourceType(e.SourceType),
		RevokedAt: e.RevokedAt, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
	if e.SourceID != nil {
		rec.SourceID = billing.SourceRef(string(e.SourceType), e.SourceID.String())
	}
	if e.RevokeReason != nil {
		reason := string(*e.RevokeReason)
		rec.RevokeReason = &reason
	}
	return rec
}

// tenantSubjectForEntitlementGrantTarget resolves the payable customer for an
// admin entitlement-grant TARGET. #528: the admin surface addresses users by
// their UUID user_id and READS them by that same id (GetAdminUserBillingProfile
// → identity.CustomerIDFromString; EntitlementService.ListActiveRecords), exactly
// as commerce writers resolve a payer (RegisterPurchase → customerIDFromUser). A
// grant must therefore land on the SAME #364 UUID subject, or an admin could not
// see the entitlement they just granted. Resolution is identical for delegated
// and non-delegated callers; the merchant comes from the pinned request context.
// (This replaces the old delegated-only federated (issuer, subject) mint, which
// produced a different customer id than every read on the admin surface — the
// "wrong assumption" #528 removes.)
func tenantSubjectForEntitlementGrantTarget(r *httprequest.Request, subject string) (uuid.UUID, error) {
	return db.EnsureCustomerID(r.Request.Context(), r.State.DB.Qx(r.Request.Context()), uuid.Nil, subject)
}

// DeleteEntitlement revokes one of the customer's entitlement windows. The
// merchant's own grant is revoked in the grant ledger, so its future windows
// end with it; a window from a purchase or subscription is revoked on its own.
func DeleteEntitlement(r *httprequest.Request) {
	customerID := customerIDParam(r.Param("customer_id"))
	id, err := billing.ParseEntitlementID(r.Param("id"))
	if customerID.IsZero() || err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid entitlement id").WithParam("id"))
		return
	}
	svc := r.State.EntitlementService
	ent, err := svc.GetByID(r.Request.Context(), id.UUID())
	if err != nil || ent.CustomerID != customerID.UUID() {
		r.APIError(api.Coded(billing.CodeResourceNotFound, "entitlement not found"))
		return
	}
	if err := svc.Revoke(r.Request.Context(), ent, models.EntitlementRevokeAdmin); err != nil {
		r.InternalError("failed to revoke entitlement", err)
		return
	}
	convergeAfterMutation(r, ent.CustomerID)
	r.Status(http.StatusNoContent)
}
