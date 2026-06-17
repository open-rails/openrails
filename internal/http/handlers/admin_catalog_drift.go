package handlers

import (
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Catalog drift handlers (issue #209/#510). Mounted under /merchant/catalog/*
// with the live openrails:catalog:write permission gate. These surface the alert-only catalog
// reconciliation loop's findings; they never mutate Stripe or the catalog rows.
// Operators resolve drift through the existing per-price reconcile action.

// AdminListCatalogDrift lists open (unresolved) catalog drift events with
// pagination and optional provider / kind / resource_type filters. The
// provider filter ('stripe' | 'nmi') disambiguates the shared field_drift kind.
//
//	GET /merchant/catalog/drift?provider=nmi&kind=field_drift&resource_type=price&limit=&offset=
func AdminListCatalogDrift(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	filter := billingservice.CatalogDriftFilter{
		Provider:     strings.TrimSpace(r.Query("provider")),
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

// AdminListCatalogOrphans lists open orphan drift events across providers. The
// optional ?provider= filter ('stripe' | 'nmi') scopes it to one processor and
// selects the matching orphan kind; with no provider it returns orphans from
// every provider. (CCBill is never reconciled — no catalog-list API — so it
// never appears here.)
//
//	GET /merchant/catalog/orphans?provider=nmi&resource_type=price
func AdminListCatalogOrphans(r *httprequest.Request) {
	provider := strings.TrimSpace(r.Query("provider"))
	listOrphans(r, provider)
}

// AdminListStripeOrphans is the operator-friendly convenience alias for
// GET /merchant/catalog/orphans?provider=stripe.
//
//	GET /merchant/catalog/stripe/orphans
func AdminListStripeOrphans(r *httprequest.Request) {
	listOrphans(r, "stripe")
}

// listOrphans lists open orphan events, optionally scoped to a single provider.
// An empty provider returns orphans from all providers (no kind filter would
// match both orphan_in_stripe and orphan_in_nmi, so it filters by kind suffix
// in the service via two narrowing queries collapsed here into a provider-only
// scope when given, and an unfiltered-kind list otherwise).
func listOrphans(r *httprequest.Request, provider string) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	kind := ""
	switch strings.ToLower(provider) {
	case "stripe":
		kind = "orphan_in_stripe"
	case "nmi":
		kind = "orphan_in_nmi"
	}
	filter := billingservice.CatalogDriftFilter{
		Provider:     provider,
		Kind:         kind,
		ResourceType: strings.TrimSpace(r.Query("resource_type")),
		Limit:        parseIntDefault(r.Query("limit"), 100),
		Offset:       parseIntDefault(r.Query("offset"), 0),
	}
	items, total, err := svc.ListCatalogDrift(r.Request.Context(), filter)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	// When no provider is given we cannot pin a single orphan kind, so post-filter
	// to the two orphan kinds here.
	if provider == "" {
		filtered := make([]billingservice.CatalogDriftEventView, 0, len(items))
		for _, it := range items {
			if it.Kind == "orphan_in_stripe" || it.Kind == "orphan_in_nmi" {
				filtered = append(filtered, it)
			}
		}
		items = filtered
		total = int64(len(filtered))
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
