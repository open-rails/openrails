package handlers

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/catalogrules"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func ListMeters(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListUsageMeters(r.Request.Context(), page)
	if err != nil {
		writeMeteringError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func GetMeter(r *httprequest.Request) {
	meter, ok := loadMeter(r)
	if !ok {
		return
	}
	r.JSON(http.StatusOK, meter)
}

// ListMeterRateOverrides lists the customers whose negotiated price replaces
// the meter's rate card.
func ListMeterRateOverrides(r *httprequest.Request) {
	key, ok := meterKeyParam(r, "key")
	if !ok {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListUsageMeterOverrides(r.Request.Context(), key, page)
	if err != nil {
		writeMeteringError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// SetMeter declares the meter under the path's key.
func SetMeter(r *httprequest.Request) {
	var params billing.SetMeterParams
	if !r.BindJSON(&params) {
		return
	}
	meter := catalogrules.Meter{Key: r.Param("key"), EventType: params.EventType, ValueProperty: params.ValueProperty, Aggregation: params.Aggregation, Unit: params.Unit, GroupBy: params.GroupBy}
	if err := catalogrules.ValidateMeter("meter", &meter); err != nil {
		writeMeteringValidationError(r, "usage_meter_invalid", err)
		return
	}
	if !catalogrules.BillingSupported(meter.Aggregation) {
		writeMeteringValidationError(r, "usage_meter_invalid", errors.New("meter aggregation must be sum or count"))
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	if err := svc.EnsureUsageMeter(r.Request.Context(), meter); err != nil {
		writeMeteringError(r, err)
		return
	}
	stored, err := svc.GetUsageMeter(r.Request.Context(), meter.Key)
	if err != nil {
		writeMeteringError(r, err)
		return
	}
	r.JSON(http.StatusOK, stored)
}

// SetMeterRateCard sets the rate card that prices the meter's usage.
func SetMeterRateCard(r *httprequest.Request) {
	var params billing.SetMeterRateCardParams
	if !r.BindJSON(&params) {
		return
	}
	meter, ok := loadMeter(r)
	if !ok {
		return
	}
	if params.ProductID.IsZero() {
		writeMeteringValidationError(r, "usage_rate_card_invalid", errors.New("product_id required"))
		return
	}
	if params.Filter == nil {
		params.Filter = map[string][]string{}
	}
	for _, check := range []func() error{
		func() error { return catalogrules.ValidateUsagePrice("rate card", &params.Price) },
		func() error { return moneyutil.ValidateCurrency(params.Price.Currency) },
		func() error { return catalogrules.ValidateFilter("rate card", &params.Filter) },
		func() error { return catalogrules.ValidateAllowance("rate card", params.Allowance) },
		func() error {
			return catalogrules.ValidateDimensions("rate card", meter.GroupBy, params.Filter, &params.Price)
		},
	} {
		if err := check(); err != nil {
			writeMeteringValidationError(r, "usage_rate_card_invalid", err)
			return
		}
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	productID := params.ProductID.UUID()
	if err := svc.SetUsageRateCard(r.Request.Context(), billingservice.UsageRateCardInput{
		ProductID: &productID, MeterKey: meter.Key, Filter: params.Filter, Price: params.Price, Allowance: params.Allowance,
	}); err != nil {
		writeMeteringError(r, err)
		return
	}
	stored, err := svc.GetUsageMeter(r.Request.Context(), meter.Key)
	if err != nil {
		writeMeteringError(r, err)
		return
	}
	r.JSON(http.StatusOK, stored)
}

// DeleteMeterRateCard removes the meter's rate card once no customer has an
// override of it.
func DeleteMeterRateCard(r *httprequest.Request) {
	key, ok := meterKeyParam(r, "key")
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	if err := svc.DeleteDefaultUsageRateCard(r.Request.Context(), key); err != nil {
		writeMeteringError(r, err)
		return
	}
	r.Status(http.StatusNoContent)
}

func meterKeyParam(r *httprequest.Request, name string) (string, bool) {
	key := catalogrules.NormalizeKey(r.Param(name))
	if key == "" {
		r.APIError(invalidParam(name, "meter key required"))
		return "", false
	}
	return key, true
}

func loadMeter(r *httprequest.Request) (*billing.Meter, bool) {
	key, ok := meterKeyParam(r, "key")
	if !ok {
		return nil, false
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return nil, false
	}
	meter, err := svc.GetUsageMeter(r.Request.Context(), key)
	if err != nil {
		writeMeteringError(r, err)
		return nil, false
	}
	return meter, true
}

func writeMeteringValidationError(r *httprequest.Request, code string, err error) {
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, code, err.Error()))
}

func writeMeteringError(r *httprequest.Request, err error) {
	var status int
	var code string
	switch {
	case errors.Is(err, billingservice.ErrUsageMeterNotFound):
		status, code = http.StatusNotFound, "usage_meter_not_found"
	case errors.Is(err, billingservice.ErrDefaultRateCardNotFound):
		status, code = http.StatusNotFound, "default_rate_card_not_found"
	case errors.Is(err, billingservice.ErrRateCardProductNotFound):
		status, code = http.StatusNotFound, "rate_card_product_not_found"
	case errors.Is(err, billingservice.ErrAllowanceMeterNotFound):
		status, code = http.StatusNotFound, "allowance_meter_not_found"
	case errors.Is(err, billingservice.ErrUsageRateCardInvalid):
		status, code = http.StatusBadRequest, "usage_rate_card_invalid"
	case errors.Is(err, billingservice.ErrMeterInUse):
		status, code = http.StatusConflict, "meter_in_use"
	case errors.Is(err, billingservice.ErrMeterRateCardConflict):
		status, code = http.StatusConflict, "meter_rate_card_conflict"
	case errors.Is(err, billingservice.ErrAllowanceSourceInvalid):
		status, code = http.StatusConflict, "allowance_source_invalid"
	case errors.Is(err, billingservice.ErrAllowanceSourceInUse):
		status, code = http.StatusConflict, "allowance_source_in_use"
	case errors.Is(err, billingservice.ErrDefaultRateCardRequired):
		status, code = http.StatusConflict, "default_rate_card_required"
	case errors.Is(err, billingservice.ErrRateCardHasOverrides):
		status, code = http.StatusConflict, "rate_card_has_overrides"
	case errors.Is(err, billingservice.ErrRateCardCurrencyMismatch):
		status, code = http.StatusConflict, "rate_card_currency_mismatch"
	default:
		writeRefusal(r, err, "metering operation failed")
		return
	}
	r.APIError(api.NewAPIError(status, api.ErrorTypeInvalidRequest, code, err.Error()))
}
