package handlers

import (
	"errors"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogpublish"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/service"
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
		if errors.Is(err, service.ErrMeterInUse) || errors.Is(err, service.ErrRateCardHasOverrides) || errors.Is(err, service.ErrAllowanceSourceInUse) || errors.Is(err, service.ErrRateCardCurrencyMismatch) || errors.Is(err, service.ErrAllowanceSourceInvalid) || errors.Is(err, service.ErrUsageRateCardInvalid) || errors.Is(err, service.ErrMeterRateCardConflict) {
			writeMeteringError(r, err)
		} else {
			writeCatalogError(r, err)
		}
		return
	}
	r.JSON(http.StatusOK, response)
}
