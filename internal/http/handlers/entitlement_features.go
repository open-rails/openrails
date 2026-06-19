package handlers

import (
	"net/http"
	"strings"
	"time"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Active-entitlements SELF read (issue #245). #528 retired the admin
// feature/product-feature CRUD surface (it was catalog/feature-definition, not
// per-user billing admin); only the self read remains, surfacing a user's active
// entitlements. Admin views embed entitlements via the user-detail composite.

func featureService(r *httprequest.Request) *entitlements.FeatureService {
	if r.State == nil {
		return nil
	}
	return r.State.FeatureService
}

// SelfGetActiveEntitlements handles the delegated self surface read. It derives
// the acting user from the authenticated identity rather than accepting an
// arbitrary user_id.
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
