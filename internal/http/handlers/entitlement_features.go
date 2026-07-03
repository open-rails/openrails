package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Active-entitlements SELF read (issue #245). #528 retired the admin
// feature/product-feature CRUD surface; #702 dropped the feature-definition
// tables entirely — entitlements are plain strings (product.EntitlementsSpec
// keys). The openrails.entitlements window ledger is the source of truth.

// activeEntitlement is one active entitlement window: the entitlement string
// (lookup_key) plus its window/source fields.
type activeEntitlement struct {
	ID         uuid.UUID                    `json:"id"`
	CustomerID string                       `json:"customer_id"`
	LookupKey  string                       `json:"lookup_key"`
	StartAt    time.Time                    `json:"start_at"`
	EndAt      *time.Time                   `json:"end_at,omitempty"`
	SourceType models.EntitlementSourceType `json:"source_type"`
	SourceID   *uuid.UUID                   `json:"source_id,omitempty"`
}

// SelfGetActiveEntitlements handles the delegated self surface read. It derives
// the acting user from the authenticated identity rather than accepting an
// arbitrary user_id.
func SelfGetActiveEntitlements(r *httprequest.Request) {
	if r.State == nil || r.State.EntitlementService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "entitlement service unavailable")
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
	if at.IsZero() {
		at = time.Now().UTC()
	}
	windows, err := r.State.EntitlementService.ListActiveRecords(r.Request.Context(), user.ID, at)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to resolve active entitlements")
		return
	}
	items := make([]activeEntitlement, 0, len(windows))
	for _, w := range windows {
		items = append(items, activeEntitlement{
			ID:         w.ID,
			CustomerID: w.CustomerID.String(),
			LookupKey:  w.Entitlement,
			StartAt:    w.StartAt,
			EndAt:      w.EndAt,
			SourceType: w.SourceType,
			SourceID:   w.SourceID,
		})
	}
	// Stripe-shaped list envelope: object=list, has_more, data[].
	r.JSON(http.StatusOK, map[string]any{
		"object":   "list",
		"has_more": false,
		"data":     items,
	})
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
		r.ErrorJSON(http.StatusBadRequest, "invalid 'at' timestamp format; use RFC3339")
		return time.Time{}, false
	}
	return parsed, true
}
