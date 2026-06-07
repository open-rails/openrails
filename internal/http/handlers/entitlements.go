package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/open-rails/openrails/pkg/tenant"
)

type ServiceEntitlementRecord struct {
	ID              string     `json:"id"`
	TenantSubjectID string     `json:"tenant_subject_id,omitempty"`
	Entitlement     string     `json:"entitlement"`
	StartAt         time.Time  `json:"start_at"`
	EndAt           *time.Time `json:"end_at,omitempty"`
	SourceID        *string    `json:"source_id,omitempty"`
	SourceType      string     `json:"source_type"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	RevokeReason    *string    `json:"revoke_reason,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
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

func ServiceGetTenantSubjectEntitlements(r *httprequest.Request) {
	tenantSubject, err := parseServiceTenantSubjectID(r.Param("tenant_subject_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if tenantSubject == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
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
	entitlements, err := svc.ListActiveEntitlementRecordsForTenantSubject(r.Request.Context(), *tenantSubject, queryTime)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to fetch entitlements")
		return
	}
	r.JSON(http.StatusOK, serviceEntitlementRecordsFromService(entitlements))
}

func ServiceGetExternalSubjectEntitlements(r *httprequest.Request) {
	issuer := strings.TrimSpace(r.Query("issuer"))
	subject := strings.TrimSpace(r.Query("subject"))
	if issuer == "" {
		r.ErrorJSON(http.StatusBadRequest, "issuer required")
		return
	}
	if subject == "" {
		r.ErrorJSON(http.StatusBadRequest, "subject required")
		return
	}
	at, ok := serviceEntitlementQueryTime(r)
	if !ok {
		return
	}

	var tenantSubjectID uuid.UUID
	err := r.State.DB.Q(r.Request.Context()).NewRaw(`
		SELECT id
		  FROM billing.tenant_subjects
		 WHERE tenant_id = ?
		   AND issuer = ?
		   AND subject = ?
		 LIMIT 1
	`, tenant.FromContextOrDefault(r.Request.Context()).UUID(), issuer, subject).Scan(r.Request.Context(), &tenantSubjectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			r.JSON(http.StatusOK, []ServiceEntitlementRecord{})
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to resolve tenant subject")
		return
	}
	if !requireServiceTenantSubjectScope(r, identity.TenantSubjectID(tenantSubjectID)) {
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	entitlements, err := svc.ListActiveEntitlementRecordsForTenantSubject(r.Request.Context(), identity.TenantSubjectID(tenantSubjectID), at)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to fetch entitlements")
		return
	}
	r.JSON(http.StatusOK, serviceEntitlementRecordsFromService(entitlements))
}

func serviceEntitlementQueryTime(r *httprequest.Request) (time.Time, bool) {
	atStr := strings.TrimSpace(r.Query("at"))
	if atStr != "" {
		parsed, err := timeutil.ParseRFC3339UTC(atStr)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid 'at' timestamp format; use RFC3339")
			return time.Time{}, false
		}
		return parsed, true
	}
	return r.Clock.Now(), true
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
	tenantSubjectID, err := tenantSubjectForAdminGrantTarget(r, path.UserID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to resolve target tenant subject")
		return
	}
	now := time.Now()
	if r.State.Clock != nil {
		now = r.State.Clock.Now()
	}
	adminGrant := &models.AdminGrant{ID: uuidutil.NewV7(), TenantSubjectID: tenantSubjectID, GrantedBy: adminUser.ID, Reason: "admin_entitlement", DurationDays: req.Days, CreatedAt: now}
	if _, err := r.State.DB.Q(r.Request.Context()).NewInsert().Model(adminGrant).Exec(r.Request.Context()); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to create admin grant source record")
		return
	}
	var ent *models.Entitlement
	if req.Days != nil {
		d := time.Duration(*req.Days) * 24 * time.Hour
		ent, err = svc.PushNewEntitlement(r.Request.Context(), entitlements.PushNewEntitlementParams{UserID: path.UserID, TenantSubjectID: tenantSubjectID, Entitlement: req.Entitlement, Duration: &d, SourceType: models.EntitlementSourceAdmin, SourceID: adminGrant.ID})
	} else {
		ent, err = svc.PushNewEntitlement(r.Request.Context(), entitlements.PushNewEntitlementParams{UserID: path.UserID, TenantSubjectID: tenantSubjectID, Entitlement: req.Entitlement, Indefinite: true, SourceType: models.EntitlementSourceAdmin, SourceID: adminGrant.ID})
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.JSON(http.StatusCreated, ent)
}

func tenantSubjectForAdminGrantTarget(r *httprequest.Request, subject string) (uuid.UUID, error) {
	value, ok := r.Get(ginmw.DelegatedContextKey)
	if !ok {
		return uuid.Nil, nil
	}
	resolved, ok := value.(*controlplane.ResolvedDelegated)
	if !ok || resolved == nil {
		return uuid.Nil, errors.New("delegated state invalid")
	}
	issuer := strings.TrimSpace(resolved.Issuer)
	subject = strings.TrimSpace(subject)
	if resolved.TenantID.IsZero() || issuer == "" || subject == "" {
		return uuid.Nil, controlplane.ErrTenantSubjectInvalid
	}
	var id uuid.UUID
	err := r.State.DB.Q(r.Request.Context()).NewRaw(`
		INSERT INTO billing.tenant_subjects (tenant_id, issuer, subject)
		VALUES (?, ?, ?)
		ON CONFLICT (tenant_id, issuer, subject) DO UPDATE
		   SET last_seen_at = now()
		RETURNING id
	`, resolved.TenantID.UUID(), issuer, subject).Scan(r.Request.Context(), &id)
	return id, err
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
	if ent.TenantSubjectID.String() != path.UserID {
		r.ErrorJSON(http.StatusNotFound, "entitlement not found for this user")
		return
	}
	if err := svc.RevokeExistingEntitlement(r.Request.Context(), entitlements.RevokeExistingEntitlementParams{EntitlementID: &entitlementID, Reason: models.EntitlementRevokeAdmin}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSONMessage("entitlement revoked")
}

func serviceEntitlementRecordsFromModels(entitlements []models.Entitlement) []ServiceEntitlementRecord {
	result := make([]ServiceEntitlementRecord, 0, len(entitlements))
	for _, e := range entitlements {
		rec := ServiceEntitlementRecord{ID: e.ID.String(), TenantSubjectID: e.TenantSubjectID.String(), Entitlement: e.Entitlement, StartAt: e.StartAt, SourceType: string(e.SourceType), CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
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
		rec := ServiceEntitlementRecord{ID: e.ID.String(), TenantSubjectID: e.TenantSubjectID.String(), Entitlement: e.Entitlement, StartAt: e.StartAt, SourceType: e.SourceType, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
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
