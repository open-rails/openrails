package handlers

import (
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Admin catalog drift handlers (issue #209). Mounted under /admin/catalog/* with
// the OperatorAdminRequired middleware. These surface the alert-only catalog
// reconciliation loop's findings; they never mutate Stripe or the catalog rows.
// Operators resolve drift through the existing per-price reconcile action.

// AdminListCatalogDrift lists open (unresolved) catalog drift events with
// pagination and optional kind / resource_type filters.
//
//	GET /admin/catalog/drift?kind=field_drift&resource_type=price&limit=&offset=
func AdminListCatalogDrift(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	filter := billingservice.CatalogDriftFilter{
		Kind:         strings.TrimSpace(r.GinCtx.Query("kind")),
		ResourceType: strings.TrimSpace(r.GinCtx.Query("resource_type")),
		Limit:        parseIntDefault(r.GinCtx.Query("limit"), 100),
		Offset:       parseIntDefault(r.GinCtx.Query("offset"), 0),
	}
	items, total, err := svc.ListCatalogDrift(r.Request.Context(), filter)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.GinCtx.JSON(http.StatusOK, paginatedResponse[billingservice.CatalogDriftEventView]{
		Items:  items,
		Total:  total,
		Limit:  filter.Limit,
		Offset: filter.Offset,
	})
}

// AdminListStripeOrphans is an operator-friendly alias for
// GET /admin/catalog/drift?kind=orphan_in_stripe.
//
//	GET /admin/catalog/stripe/orphans
func AdminListStripeOrphans(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	filter := billingservice.CatalogDriftFilter{
		Kind:         "orphan_in_stripe",
		ResourceType: strings.TrimSpace(r.GinCtx.Query("resource_type")),
		Limit:        parseIntDefault(r.GinCtx.Query("limit"), 100),
		Offset:       parseIntDefault(r.GinCtx.Query("offset"), 0),
	}
	items, total, err := svc.ListCatalogDrift(r.Request.Context(), filter)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.GinCtx.JSON(http.StatusOK, paginatedResponse[billingservice.CatalogDriftEventView]{
		Items:  items,
		Total:  total,
		Limit:  filter.Limit,
		Offset: filter.Offset,
	})
}

// AdminRefreshCatalogDrift runs the pull-and-diff pass synchronously on demand
// and returns the resulting open drift set. Idempotent.
//
//	POST /admin/catalog/drift/refresh
func AdminRefreshCatalogDrift(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	report, err := svc.RunCatalogReconciliation(r.Request.Context())
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.GinCtx.JSON(http.StatusOK, report)
}
