package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Stripe-shaped entitlement feature handlers (issue #245). These expose the
// first-class feature + product-feature layer that sits on top of OpenRails'
// temporal entitlement-window ledger. Feature/product-feature writes are admin
// operations; the active-entitlements read is a service/self read.

type createFeatureRequest struct {
	LookupKey string         `json:"lookup_key" binding:"required"`
	Name      string         `json:"name" binding:"required"`
	Active    *bool          `json:"active,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type attachFeatureRequest struct {
	EntitlementFeatureID string         `json:"entitlement_feature_id" binding:"required"`
	DurationDays         *int           `json:"duration_days,omitempty"`
	Metadata             map[string]any `json:"metadata,omitempty"`
}

func featureService(r *httprequest.Request) *entitlements.FeatureService {
	if r.State == nil {
		return nil
	}
	return r.State.FeatureService
}

// CreateEntitlementFeature handles POST /v1/admin/entitlements/features.
func CreateEntitlementFeature(r *httprequest.Request) {
	svc := featureService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "feature service unavailable")
		return
	}
	var req createFeatureRequest
	if !r.BindJSON(&req) {
		return
	}
	f, err := svc.CreateFeature(r.Request.Context(), entitlements.CreateFeatureParams{
		LookupKey: req.LookupKey,
		Name:      req.Name,
		Active:    req.Active,
		Metadata:  req.Metadata,
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.JSON(http.StatusCreated, f)
}

// ListEntitlementFeatures handles GET /v1/admin/entitlements/features.
func ListEntitlementFeatures(r *httprequest.Request) {
	svc := featureService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "feature service unavailable")
		return
	}
	features, err := svc.ListFeatures(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list features")
		return
	}
	r.JSON(http.StatusOK, gin_h_list("entitlements.feature", features))
}

// ListProductFeatures handles GET /v1/admin/products/:id/features.
func ListProductFeatures(r *httprequest.Request) {
	svc := featureService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "feature service unavailable")
		return
	}
	productID, ok := parseUUIDParam(r, "id", "invalid product id")
	if !ok {
		return
	}
	features, err := svc.ListProductFeatures(r.Request.Context(), productID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list product features")
		return
	}
	r.JSON(http.StatusOK, gin_h_list("product_feature", features))
}

// AttachProductFeature handles POST /v1/admin/products/:id/features.
func AttachProductFeature(r *httprequest.Request) {
	svc := featureService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "feature service unavailable")
		return
	}
	productID, ok := parseUUIDParam(r, "id", "invalid product id")
	if !ok {
		return
	}
	var req attachFeatureRequest
	if !r.BindJSON(&req) {
		return
	}
	featureID, err := uuid.Parse(strings.TrimSpace(req.EntitlementFeatureID))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid entitlement_feature_id")
		return
	}
	pef, err := svc.AttachFeatureToProduct(r.Request.Context(), entitlements.AttachFeatureParams{
		ProductID:            productID,
		EntitlementFeatureID: featureID,
		DurationDays:         req.DurationDays,
		Metadata:             req.Metadata,
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.JSON(http.StatusCreated, pef)
}

// DetachProductFeature handles DELETE /v1/admin/products/:id/features/:product_feature_id.
func DetachProductFeature(r *httprequest.Request) {
	svc := featureService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "feature service unavailable")
		return
	}
	pfID, ok := parseUUIDParam(r, "product_feature_id", "invalid product_feature_id")
	if !ok {
		return
	}
	if err := svc.DetachFeature(r.Request.Context(), pfID); err != nil {
		r.ErrorJSON(http.StatusNotFound, err.Error())
		return
	}
	r.SuccessJSONMessage("product feature detached")
}

// ServiceGetActiveEntitlements handles
// GET /v1/service/entitlements/active_entitlements?user_id=...
// for server-to-server callers (service token-gated). It returns the Stripe-shaped active
// entitlements for the given user.
func ServiceGetActiveEntitlements(r *httprequest.Request) {
	svc := featureService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "feature service unavailable")
		return
	}
	userID := strings.TrimSpace(r.Query("user_id"))
	if userID == "" {
		r.ErrorJSON(http.StatusBadRequest, "user_id is required")
		return
	}
	if _, err := uuid.Parse(userID); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid user_id format")
		return
	}
	at, ok := parseAtQuery(r)
	if !ok {
		return
	}
	writeActiveEntitlements(r, svc, userID, at)
}

// SelfGetActiveEntitlements handles GET /v1/me/entitlements/active (or the
// delegated self surface). It derives the acting user from the authenticated
// identity rather than accepting an arbitrary user_id.
func SelfGetActiveEntitlements(r *httprequest.Request) {
	svc := featureService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "feature service unavailable")
		return
	}
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "missing user identity")
		return
	}
	at, ok := parseAtQuery(r)
	if !ok {
		return
	}
	writeActiveEntitlements(r, svc, user.ID, at)
}

func writeActiveEntitlements(r *httprequest.Request, svc *entitlements.FeatureService, userID string, at time.Time) {
	items, err := svc.GetActiveEntitlements(r.Request.Context(), userID, at)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to resolve active entitlements")
		return
	}
	// Stripe-shaped list envelope: object=list, has_more, data[].
	r.JSON(http.StatusOK, map[string]any{
		"object":   "list",
		"has_more": false,
		"data":     items,
	})
}

// parseAtQuery reads an optional RFC3339 `at` query param. When absent, returns a
// zero time (the service defaults it to now).
func parseAtQuery(r *httprequest.Request) (time.Time, bool) {
	atStr := strings.TrimSpace(r.Query("at"))
	if atStr == "" {
		return time.Time{}, true
	}
	parsed, err := timeutil.ParseRFC3339UTC(atStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid 'at' timestamp format; use RFC3339")
		return time.Time{}, false
	}
	return parsed, true
}

func parseUUIDParam(r *httprequest.Request, name, msg string) (uuid.UUID, bool) {
	raw := strings.TrimSpace(r.Param(name))
	id, err := uuid.Parse(raw)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, msg)
		return uuid.UUID{}, false
	}
	return id, true
}

// gin_h_list wraps a slice in the Stripe-shaped list envelope.
func gin_h_list[T any](object string, data []T) map[string]any {
	if data == nil {
		data = []T{}
	}
	return map[string]any{
		"object":   "list",
		"has_more": false,
		"data":     data,
	}
}
