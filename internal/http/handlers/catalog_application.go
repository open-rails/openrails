package handlers

import (
	"io"
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/catalog"
)

func MerchantApplyCatalog(r *httprequest.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Request.Body, catalog.MaxApplicationBytes+1))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "could not read catalog application")
		return
	}
	var application *catalog.Application
	contentType := strings.TrimSpace(strings.SplitN(r.Request.Header.Get("Content-Type"), ";", 2)[0])
	switch contentType {
	case "application/yaml", "application/x-yaml", "text/yaml":
		application, err = catalog.ParseApplicationYAML(body)
	case "application/json", "":
		application, err = catalog.ParseApplicationJSON(body)
	default:
		r.ErrorJSON(http.StatusUnsupportedMediaType, "catalog applications require JSON or YAML")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	receipt, err := svc.ApplyCatalog(r.Request.Context(), *application)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, receipt)
}

func MerchantCatalogRevision(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	revision, err := svc.CatalogRevision(r.Request.Context())
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	_, allowed := adminCatalogOwnership(r)
	r.JSON(http.StatusOK, openrails.CatalogRevision{Revision: revision, WritesAllowed: allowed})
}
