package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Active-entitlements SELF read (issue #245). #528 retired the admin
// feature/product-feature CRUD surface; #702 dropped the feature-definition
// tables entirely — entitlements are plain strings (product.Entitlements
// keys). The billing.entitlements window ledger is the source of truth.

// activeEntitlement is one active entitlement window: the entitlement string
// (lookup_key) plus its window/source fields. source_id is the source's own
// wire id (billing.SourceRef), as on EntitlementRecord.
type activeEntitlement struct {
	ID         uuid.UUID                    `json:"id"`
	CustomerID billing.CustomerID           `json:"customer_id"`
	LookupKey  string                       `json:"lookup_key"`
	StartsAt   time.Time                    `json:"starts_at"`
	EndsAt     *time.Time                   `json:"ends_at,omitempty"`
	SourceType models.EntitlementSourceType `json:"source_type"`
	SourceID   string                       `json:"source_id,omitempty"`
}

// SelfGetActiveEntitlements handles the delegated self surface read. It derives
// the acting user from the authenticated identity rather than accepting an
// arbitrary user_id.
func SelfGetActiveEntitlements(r *httprequest.Request) {
	if r.State == nil || r.State.EntitlementService == nil {
		r.ErrorCode(billing.CodeInternalError, "entitlement service unavailable")
		return
	}
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "missing user identity")
		return
	}
	at, ok := parseAtQuery(r)
	if !ok {
		return
	}
	if at.IsZero() {
		at = r.Clock.Now().UTC()
	}
	windows, err := r.State.EntitlementService.ListActiveRecords(r.Request.Context(), user.ID, at)
	if err != nil {
		r.ErrorCode(billing.CodeInternalError, "failed to resolve active entitlements")
		return
	}
	items := make([]activeEntitlement, 0, len(windows))
	for _, w := range windows {
		item := activeEntitlement{
			ID:         w.ID,
			CustomerID: billing.CustomerID(w.CustomerID),
			LookupKey:  w.Entitlement,
			StartsAt:   w.StartsAt,
			EndsAt:     w.EndsAt,
			SourceType: w.SourceType,
		}
		if w.SourceID != nil {
			item.SourceID = billing.SourceRef(string(w.SourceType), w.SourceID.String())
		}
		items = append(items, item)
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
		r.ErrorCode(billing.CodeInvalidParam, "invalid 'at' timestamp format; use RFC3339")
		return time.Time{}, false
	}
	return parsed, true
}
