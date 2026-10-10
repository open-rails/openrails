package handlers

import (
	"io"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// ApplyCatalog applies a catalog document, JSON or YAML.
func ApplyCatalog(r *httprequest.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Request.Body, catalog.MaxApplicationBytes+1))
	if err != nil {
		r.ErrorCode(billing.CodeInvalidRequestBody, "could not read catalog application")
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
		r.ErrorCode(billing.CodeUnsupportedMediaType, "catalog applications require JSON or YAML")
		return
	}
	if err != nil {
		r.ErrorCode(billing.CodeInvalidRequestBody, err.Error())
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

// GetCatalogRevision reads the catalog revision and whether catalog writes
// are accepted.
func GetCatalogRevision(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	revision, err := svc.CatalogRevision(r.Request.Context())
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	var exposure *catalogpolicy.Exposure
	if r.State != nil {
		exposure = r.State.CatalogEdits
	}
	allowed := catalogpolicy.Check(r.Request.Context(), exposure) == nil
	r.JSON(http.StatusOK, billing.CatalogRevision{Revision: revision, WritesAllowed: allowed})
}
