package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
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
	if _, err := converge.AfterMutation(r.Request.Context(), r.State.DB, mID, customer); err != nil {
		log.WithContext(r.Request.Context()).WithError(err).
			Warn("inline converge after mutation failed; background sweep will reconcile")
	}
}

type ServiceEntitlementRecord struct {
	ID           string     `json:"id"`
	CustomerID   string     `json:"customer_id,omitempty"`
	Entitlement  string     `json:"entitlement"`
	StartAt      time.Time  `json:"start_at"`
	EndAt        *time.Time `json:"end_at,omitempty"`
	SourceID     *string    `json:"source_id,omitempty"`
	SourceType   string     `json:"source_type"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokeReason *string    `json:"revoke_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type adminUserEntitlementsPath struct {
	UserID string `uri:"user_id" binding:"required"`
}

type adminEntitlementPath struct {
	UserID        string `uri:"user_id" binding:"required"`
	EntitlementID string `uri:"id" binding:"required"`
}

type grantEntitlementRequest struct {
	Entitlement string `json:"entitlement" binding:"required"`
	Days        *int   `json:"days,omitempty"`
}

func ServiceGetCustomerEntitlements(r *httprequest.Request) {
	tenantSubject, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	atStr := strings.TrimSpace(r.Query("at"))
	var at *time.Time
	if atStr != "" {
		parsed, err := timeutil.ParseRFC3339UTC(atStr)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid 'at' timestamp format; use RFC3339")
			return
		}
		at = &parsed
	}
	queryTime := r.Clock.Now()
	if at != nil {
		queryTime = *at
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	entitlements, err := svc.ListActiveEntitlementRecordsForCustomer(r.Request.Context(), *tenantSubject, queryTime)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to fetch entitlements")
		return
	}
	r.JSON(http.StatusOK, serviceEntitlementRecordsFromService(entitlements))
}

type serviceExternalSubjectEntitlementsRequest struct {
	Issuer   string   `json:"issuer"`
	Subjects []string `json:"subjects"`
	At       string   `json:"at,omitempty"` // RFC3339; empty = now
}

// ServiceGetExternalSubjectEntitlements is always batch (#354): one query
// answers many subjects; a single lookup is an array of one. Response is
// keyed by subject with an entry per requested subject (unknown = [], never
// an error) after trim + dedupe; over-cap is a 400.
func ServiceGetExternalSubjectEntitlements(r *httprequest.Request) {
	var req serviceExternalSubjectEntitlementsRequest
	if !r.BindJSON(&req) {
		return
	}
	issuer := strings.TrimSpace(req.Issuer)
	if issuer == "" {
		r.ErrorJSON(http.StatusBadRequest, "issuer required")
		return
	}
	subjects := make([]string, 0, len(req.Subjects))
	seen := make(map[string]struct{}, len(req.Subjects))
	for _, s := range req.Subjects {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		subjects = append(subjects, s)
	}
	if len(subjects) == 0 {
		r.ErrorJSON(http.StatusBadRequest, "subjects required")
		return
	}
	if len(subjects) > billingservice.EntitlementsBatchMaxSubjects {
		r.ErrorJSON(http.StatusBadRequest, fmt.Sprintf("too many subjects: %d > %d per call", len(subjects), billingservice.EntitlementsBatchMaxSubjects))
		return
	}
	at := r.Clock.Now()
	if raw := strings.TrimSpace(req.At); raw != "" {
		parsed, err := timeutil.ParseRFC3339UTC(raw)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid 'at' timestamp format; use RFC3339")
			return
		}
		at = parsed
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	grouped, err := svc.ListActiveEntitlementRecordsByExternalSubjects(r.Request.Context(), issuer, subjects, at)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to fetch entitlements")
		return
	}
	// A subject-scoped token reads only its own subject. All records of one
	// subject share a tenant_subject id.
	for _, records := range grouped {
		if len(records) == 0 {
			continue
		}
		if !requireServiceCustomerScope(r, identity.CustomerID(records[0].CustomerID)) {
			return
		}
	}
	out := make(map[string][]ServiceEntitlementRecord, len(subjects))
	for _, s := range subjects {
		out[s] = []ServiceEntitlementRecord{}
	}
	for subject, records := range grouped {
		out[subject] = serviceEntitlementRecordsFromService(records)
	}
	r.JSON(http.StatusOK, out)
}

func GetAdminUserEntitlements(r *httprequest.Request) {
	var path adminUserEntitlementsPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc := r.State.EntitlementService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "entitlement service unavailable")
		return
	}
	atStr := r.Query("at")
	queryTime := time.Now()
	if r.State.Clock != nil {
		queryTime = r.State.Clock.Now()
	}
	if atStr != "" {
		parsed, err := timeutil.ParseRFC3339UTC(atStr)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid 'at' timestamp format; use RFC3339")
			return
		}
		queryTime = parsed
	}
	entitlements, err := svc.ListActiveRecords(r.Request.Context(), path.UserID, queryTime)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to fetch entitlements")
		return
	}
	r.JSON(http.StatusOK, serviceEntitlementRecordsFromModels(entitlements))
}

func GrantAdminEntitlement(r *httprequest.Request) {
	var path adminUserEntitlementsPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var req grantEntitlementRequest
	if !r.BindJSON(&req) {
		return
	}
	svc := r.State.EntitlementService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "entitlement service unavailable")
		return
	}
	adminUser := r.GetUser()
	if adminUser == nil || adminUser.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "missing admin identity")
		return
	}
	if req.Days != nil && *req.Days <= 0 {
		r.ErrorJSON(http.StatusBadRequest, "days must be > 0 (or omit for indefinite)")
		return
	}
	tenantSubjectID, err := tenantSubjectForEntitlementGrantTarget(r, path.UserID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to resolve target tenant subject")
		return
	}
	// #511: a manually-granted entitlement is now just an `admin`-sourced grant in
	// the ledger (the source of truth); no separate entitlement_grants provenance
	// row. SourceID is the grant's own identity.
	adminGrantID := uuidutil.NewV7()
	var ent *models.Entitlement
	if req.Days != nil {
		d := time.Duration(*req.Days) * 24 * time.Hour
		ent, err = svc.PushNewEntitlement(r.Request.Context(), entitlements.PushNewEntitlementParams{UserID: path.UserID, CustomerID: tenantSubjectID, Entitlement: req.Entitlement, Duration: &d, SourceType: models.EntitlementSourceAdmin, SourceID: adminGrantID})
	} else {
		ent, err = svc.PushNewEntitlement(r.Request.Context(), entitlements.PushNewEntitlementParams{UserID: path.UserID, CustomerID: tenantSubjectID, Entitlement: req.Entitlement, Indefinite: true, SourceType: models.EntitlementSourceAdmin, SourceID: adminGrantID})
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	convergeAfterMutation(r, tenantSubjectID) // #511: re-converge the customer inline
	r.JSON(http.StatusCreated, ent)
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
	return repo.EnsureCustomerID(r.Request.Context(), r.State.DB.Qx(r.Request.Context()), uuid.Nil, subject)
}

func RevokeAdminEntitlement(r *httprequest.Request) {
	var path adminEntitlementPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	entitlementID, err := uuid.Parse(path.EntitlementID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid entitlement ID")
		return
	}
	svc := r.State.EntitlementService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "entitlement service unavailable")
		return
	}
	ent, err := svc.GetByID(r.Request.Context(), entitlementID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "entitlement not found")
		return
	}
	if ent.CustomerID.String() != path.UserID {
		r.ErrorJSON(http.StatusNotFound, "entitlement not found for this user")
		return
	}
	if err := svc.RevokeExistingEntitlement(r.Request.Context(), entitlements.RevokeExistingEntitlementParams{EntitlementID: &entitlementID, Reason: models.EntitlementRevokeAdmin}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	convergeAfterMutation(r, ent.CustomerID) // #511: re-converge the customer inline
	r.SuccessJSONMessage("entitlement revoked")
}

func serviceEntitlementRecordsFromModels(entitlements []models.Entitlement) []ServiceEntitlementRecord {
	result := make([]ServiceEntitlementRecord, 0, len(entitlements))
	for _, e := range entitlements {
		rec := ServiceEntitlementRecord{ID: e.ID.String(), CustomerID: e.CustomerID.String(), Entitlement: e.Entitlement, StartAt: e.StartAt, SourceType: string(e.SourceType), CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
		if e.EndAt != nil {
			rec.EndAt = e.EndAt
		}
		if e.SourceID != nil {
			sourceStr := e.SourceID.String()
			rec.SourceID = &sourceStr
		}
		if e.RevokedAt != nil {
			rec.RevokedAt = e.RevokedAt
		}
		if e.RevokeReason != nil {
			reasonStr := string(*e.RevokeReason)
			rec.RevokeReason = &reasonStr
		}
		result = append(result, rec)
	}
	return result
}

func serviceEntitlementRecordsFromService(entitlements []billingservice.EntitlementRecord) []ServiceEntitlementRecord {
	result := make([]ServiceEntitlementRecord, 0, len(entitlements))
	for _, e := range entitlements {
		rec := ServiceEntitlementRecord{ID: e.ID.String(), CustomerID: e.CustomerID.String(), Entitlement: e.Entitlement, StartAt: e.StartAt, SourceType: e.SourceType, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
		if e.EndAt != nil {
			rec.EndAt = e.EndAt
		}
		if e.SourceID != nil {
			sourceStr := e.SourceID.String()
			rec.SourceID = &sourceStr
		}
		if e.RevokedAt != nil {
			rec.RevokedAt = e.RevokedAt
		}
		if e.RevokeReason != nil {
			rec.RevokeReason = e.RevokeReason
		}
		result = append(result, rec)
	}
	return result
}
