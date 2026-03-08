package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// ServiceEntitlementRecord represents an entitlement record returned by the service API.
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

// ServiceGetUserEntitlements returns active entitlements for a user.
// This is a service-to-service endpoint using X-API-KEY auth.
// GET /v1/users/:user_id/entitlements?at=RFC3339 (on private port 8060)
func ServiceGetUserEntitlements(r *httprequest.Request) {
	userID := strings.TrimSpace(r.GinCtx.Param("user_id"))
	if userID == "" {
		r.ErrorJSON(http.StatusBadRequest, "user_id is required")
		return
	}

	// Validate user_id format (should be a UUID)
	if _, err := uuid.Parse(userID); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid user_id format")
		return
	}

	// Optional: query entitlements at a specific time
	atStr := strings.TrimSpace(r.GinCtx.Query("at"))
	var at *time.Time
	if atStr != "" {
		parsed, err := time.Parse(time.RFC3339, atStr)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid 'at' timestamp format; use RFC3339")
			return
		}
		at = &parsed
	}

	// Use current time if not specified
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

	// Convert to response format
	result := make([]ServiceEntitlementRecord, 0, len(entitlements))
	for _, e := range entitlements {
		rec := ServiceEntitlementRecord{
			ID:          e.ID.String(),
			UserID:      e.UserID,
			Entitlement: e.Entitlement,
			StartAt:     e.StartAt,
			SourceType:  e.SourceType,
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
			rec.RevokeReason = e.RevokeReason
		}
		result = append(result, rec)
	}

	r.GinCtx.JSON(http.StatusOK, result)
}
