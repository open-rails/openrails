package handlers

import (
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Catalog drift handlers (issue #209/#510). Mounted under /merchant/catalog/*
// with the live merchant:catalog:update permission gate. These surface the alert-only catalog
// reconciliation loop's findings; they never mutate Stripe or the catalog rows.
// Operators resolve drift through the existing per-price reconcile action.

// AdminListCatalogDrift lists open (unresolved) catalog drift events with
// pagination and optional provider / kind / resource_type filters. The
// provider filter ('stripe' | 'nmi') disambiguates the shared field_drift kind.
//
//	GET /merchant/catalog/drift?rail=nmi&kind=field_drift&resource_type=price&limit=&offset=
func AdminListCatalogDrift(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	filter := billingservice.CatalogDriftFilter{
		Rail:         strings.TrimSpace(r.Query("rail")),
		Kind:         strings.TrimSpace(r.Query("kind")),
		ResourceType: strings.TrimSpace(r.Query("resource_type")),
		Limit:        parseIntDefault(r.Query("limit"), 100),
		Offset:       parseIntDefault(r.Query("offset"), 0),
	}
	items, total, err := svc.ListCatalogDrift(r.Request.Context(), filter)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, paginatedResponse[billingservice.CatalogDriftEventView]{
		Items:  items,
		Total:  total,
		Limit:  filter.Limit,
		Offset: filter.Offset,
	})
}

// AdminRefreshCatalogDrift runs the pull-and-diff pass synchronously on demand
// and returns the resulting open drift set. Idempotent.
//
//	POST /merchant/catalog/drift/refresh
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
	r.JSON(http.StatusOK, report)
}
