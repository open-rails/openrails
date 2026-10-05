package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// CatalogDriftQuery filters ListCatalogDrift.
type CatalogDriftQuery struct {
	Rail         string `form:"rail"`
	Kind         string `form:"kind"`
	ResourceType string `form:"resource_type"`
}

// ListCatalogDrift lists open catalog drift findings, newest first. They are
// alerts; reconciliation never changes a PSP or the catalog.
func ListCatalogDrift(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	var query CatalogDriftQuery
	if !r.BindQuery(&query) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListCatalogDrift(r.Request.Context(), billing.CatalogDriftListParams{PageRequest: page, Rail: query.Rail, Kind: query.Kind, ResourceType: query.ResourceType})
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// RefreshCatalogDrift reads every linked PSP's catalog now and records the
// drift it finds.
func RefreshCatalogDrift(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.RunCatalogReconciliation(r.Request.Context())
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}
