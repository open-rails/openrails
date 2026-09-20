package handlers

import (
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogpublish"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"net/http"
)

// MerchantPublishCatalog handles POST /v1/merchant/catalog/publish.
func MerchantPublishCatalog(r *httprequest.Request) {
	var req openrails.CatalogPublishRequest
	if !bindCatalogJSON(r, &req) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	response, err := catalogpublish.Publish(r.Request.Context(), svc, req)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, response)
}
