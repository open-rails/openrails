package handlers

import (
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
)

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
