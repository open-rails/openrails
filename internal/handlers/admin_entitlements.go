package handlers

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
)

type AdminUserEntitlementsPath struct {
	UserID string `uri:"user_id" binding:"required"`
}

type AdminEntitlementPath struct {
	UserID        string `uri:"user_id" binding:"required"`
	EntitlementID string `uri:"id" binding:"required"`
}

// GrantEntitlementRequest is the request body for granting an entitlement
type GrantEntitlementRequest struct {
	Entitlement string `json:"entitlement" binding:"required"` // e.g., "premium"
	Days        *int   `json:"days,omitempty"`                 // Optional: number of days (nil = indefinite)
}

// GetAdminUserEntitlements returns active entitlements for a user (admin action)
// GET /v1/admin/users/:user_id/entitlements?at=RFC3339
func GetAdminUserEntitlements(r *httprequest.Request) {
	var path AdminUserEntitlementsPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	svc := r.State.EntitlementService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "entitlement service unavailable")
		return
	}

	// Optional: query at a specific time
	atStr := r.GinCtx.Query("at")
	queryTime := time.Now()
	if r.State.Clock != nil {
		queryTime = r.State.Clock.Now()
	}
	if atStr != "" {
		parsed, err := time.Parse(time.RFC3339, atStr)
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

	// Convert to response format (reusing ServiceEntitlementRecord for consistency)
	result := make([]ServiceEntitlementRecord, 0, len(entitlements))
	for _, e := range entitlements {
		rec := ServiceEntitlementRecord{
			ID:          e.ID.String(),
			UserID:      e.UserID,
			Entitlement: e.Entitlement,
			StartAt:     e.StartAt,
			SourceType:  string(e.SourceType),
			CreatedAt:   e.CreatedAt,
			UpdatedAt:   e.UpdatedAt,
		}
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

	r.GinCtx.JSON(http.StatusOK, result)
}

// GrantAdminEntitlement grants an entitlement to a user (admin action)
// POST /v1/admin/users/:user_id/entitlements
func GrantAdminEntitlement(r *httprequest.Request) {
	var path AdminUserEntitlementsPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	var req GrantEntitlementRequest
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

	// Record an admin grant as the source-of-truth for this entitlement.
	now := time.Now()
	if r.State.Clock != nil {
		now = r.State.Clock.Now()
	}
	adminGrant := &models.AdminGrant{
		ID:           uuid.New(),
		UserID:       path.UserID,
		GrantedBy:    adminUser.ID,
		Reason:       "admin_entitlement",
		DurationDays: req.Days,
		CreatedAt:    now,
	}
	if _, err := r.State.DB.GetDB().NewInsert().Model(adminGrant).Exec(r.Request.Context()); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to create admin grant source record")
		return
	}

	var ent *models.Entitlement
	var err error
	if req.Days != nil {
		d := time.Duration(*req.Days) * 24 * time.Hour
		ent, err = svc.PushNewEntitlement(r.Request.Context(), services.PushNewEntitlementParams{
			UserID:      path.UserID,
			Entitlement: req.Entitlement,
			Duration:    &d,
			SourceType:  models.EntitlementSourceAdmin,
			SourceID:    adminGrant.ID,
		})
	} else {
		ent, err = svc.PushNewEntitlement(r.Request.Context(), services.PushNewEntitlementParams{
			UserID:      path.UserID,
			Entitlement: req.Entitlement,
			Indefinite:  true,
			SourceType:  models.EntitlementSourceAdmin,
			SourceID:    adminGrant.ID,
		})
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	r.Inner().JSON(http.StatusCreated, ent)
}

// RevokeAdminEntitlement revokes an entitlement immediately (admin action)
// DELETE /v1/admin/users/:user_id/entitlements/:id
func RevokeAdminEntitlement(r *httprequest.Request) {
	var path AdminEntitlementPath
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

	// Verify the entitlement belongs to the specified user
	ent, err := svc.GetByID(r.Request.Context(), entitlementID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "entitlement not found")
		return
	}
	if ent.UserID != path.UserID {
		r.ErrorJSON(http.StatusNotFound, "entitlement not found for this user")
		return
	}

	if err := svc.RevokeExistingEntitlement(r.Request.Context(), services.RevokeExistingEntitlementParams{
		EntitlementID: &entitlementID,
		Reason:        models.EntitlementRevokeAdmin,
	}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	r.SuccessJSONMessage("entitlement revoked")
}
