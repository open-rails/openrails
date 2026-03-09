package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type ServiceEntitlementRecord struct {
	ID           string     `json:"id"`
	UserID       string     `json:"user_id"`
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

func ServiceGetUserEntitlements(r *httprequest.Request) {
	userID := strings.TrimSpace(r.GinCtx.Param("user_id"))
	if userID == "" {
		r.ErrorJSON(http.StatusBadRequest, "user_id is required")
		return
	}
	if _, err := uuid.Parse(userID); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid user_id format")
		return
	}
	atStr := strings.TrimSpace(r.GinCtx.Query("at"))
	var at *time.Time
	if atStr != "" {
		parsed, err := timeutil.ParseRFC3339UTC(atStr)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid 'at' timestamp format; use RFC3339")
			return
		}
		at = &parsed
	}
	queryTime := time.Now()
	if at != nil {
		queryTime = *at
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	entitlements, err := svc.ListActiveEntitlementRecords(r.Request.Context(), userID, queryTime)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to fetch entitlements")
		return
	}
	r.GinCtx.JSON(http.StatusOK, serviceEntitlementRecordsFromService(entitlements))
}

func GetAdminUserEntitlements(r *httprequest.Request) {
	var path adminUserEntitlementsPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc := r.State.EntitlementService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "entitlement service unavailable")
		return
	}
	atStr := r.GinCtx.Query("at")
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
	r.GinCtx.JSON(http.StatusOK, serviceEntitlementRecordsFromModels(entitlements))
}

func GrantAdminEntitlement(r *httprequest.Request) {
	var path adminUserEntitlementsPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
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
	now := time.Now()
	if r.State.Clock != nil {
		now = r.State.Clock.Now()
	}
	adminGrant := &models.AdminGrant{ID: uuid.New(), UserID: path.UserID, GrantedBy: adminUser.ID, Reason: "admin_entitlement", DurationDays: req.Days, CreatedAt: now}
	if _, err := r.State.DB.GetDB().NewInsert().Model(adminGrant).Exec(r.Request.Context()); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to create admin grant source record")
		return
	}
	var ent *models.Entitlement
	var err error
	if req.Days != nil {
		d := time.Duration(*req.Days) * 24 * time.Hour
		ent, err = svc.PushNewEntitlement(r.Request.Context(), services.PushNewEntitlementParams{UserID: path.UserID, Entitlement: req.Entitlement, Duration: &d, SourceType: models.EntitlementSourceAdmin, SourceID: adminGrant.ID})
	} else {
		ent, err = svc.PushNewEntitlement(r.Request.Context(), services.PushNewEntitlementParams{UserID: path.UserID, Entitlement: req.Entitlement, Indefinite: true, SourceType: models.EntitlementSourceAdmin, SourceID: adminGrant.ID})
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.Inner().JSON(http.StatusCreated, ent)
}

func RevokeAdminEntitlement(r *httprequest.Request) {
	var path adminEntitlementPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
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
	if ent.UserID != path.UserID {
		r.ErrorJSON(http.StatusNotFound, "entitlement not found for this user")
		return
	}
	if err := svc.RevokeExistingEntitlement(r.Request.Context(), services.RevokeExistingEntitlementParams{EntitlementID: &entitlementID, Reason: models.EntitlementRevokeAdmin}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSONMessage("entitlement revoked")
}

func serviceEntitlementRecordsFromModels(entitlements []models.Entitlement) []ServiceEntitlementRecord {
	result := make([]ServiceEntitlementRecord, 0, len(entitlements))
	for _, e := range entitlements {
		rec := ServiceEntitlementRecord{ID: e.ID.String(), UserID: e.UserID, Entitlement: e.Entitlement, StartAt: e.StartAt, SourceType: string(e.SourceType), CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
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
		rec := ServiceEntitlementRecord{ID: e.ID.String(), UserID: e.UserID, Entitlement: e.Entitlement, StartAt: e.StartAt, SourceType: e.SourceType, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
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
