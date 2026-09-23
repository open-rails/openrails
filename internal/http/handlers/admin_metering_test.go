package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/pricing"
)

func TestUsageMeterSpec(t *testing.T) {
	spec, err := usageMeterSpec(" Storage.GB ", adminUsageMeterRequest{
		EventType:     " storage.used ",
		ValueProperty: " bytes ",
		Aggregation:   " SUM ",
		Unit:          " GB ",
		GroupBy:       map[string]string{" region ": " metadata.region "},
	})
	require.NoError(t, err)
	require.Equal(t, "storage-gb", spec.Key)
	require.Equal(t, "storage.used", spec.EventType)
	require.Equal(t, "bytes", spec.ValueProperty)
	require.Equal(t, pricing.AggregationSum, spec.Aggregation)
	require.Equal(t, "GB", spec.Unit)
	require.Equal(t, map[string]string{"region": "metadata.region"}, spec.GroupBy)

	_, err = usageMeterSpec("requests", adminUsageMeterRequest{Aggregation: pricing.AggregationMax})
	require.EqualError(t, err, "usage meter aggregation must be sum or count")
}

func TestDefaultUsageRateCardInput(t *testing.T) {
	productID := uuid.New()
	input, err := defaultUsageRateCardInput(billingservice.UsageMeterDTO{
		Key:     "storage-gb",
		GroupBy: map[string]string{"region": "metadata.region"},
	}, adminDefaultUsageRateCardRequest{
		ProductID: (openrails.ProductID(productID)).String(),
		Filter:    map[string][]string{" region ": {" eu ", "eu"}},
		Price: pricing.RatePrice{
			Model:    pricing.ModelPerUnit,
			Currency: "usd",
			PerUnit:  &pricing.PerUnitPrice{UnitAmount: 100},
		},
	})
	require.NoError(t, err)
	require.Equal(t, productID, *input.ProductID)
	require.Equal(t, "storage-gb", input.MeterKey)
	require.Equal(t, "USD", input.Price.Currency)
	require.Equal(t, map[string][]string{"region": {"eu"}}, input.Filter)

	_, err = defaultUsageRateCardInput(billingservice.UsageMeterDTO{
		Key:     "storage-gb",
		GroupBy: map[string]string{},
	}, adminDefaultUsageRateCardRequest{
		ProductID: (openrails.ProductID(productID)).String(),
		Filter:    map[string][]string{},
		Price: pricing.RatePrice{
			Model:    pricing.ModelPerUnit,
			Currency: "GBP",
			PerUnit:  &pricing.PerUnitPrice{UnitAmount: 100},
		},
	})
	require.EqualError(t, err, `money: unknown currency "GBP"`)
}

func TestAdminUsageMeterCatalogCapability(t *testing.T) {
	for _, source := range []string{config.MerchantConfigSourceManifest, config.MerchantConfigSourceAPI} {
		for _, allow := range []bool{false, true} {
			r := httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil),
				&app.Runtime{Config: &config.Config{MerchantConfigSource: source, AllowCatalogUpdates: allow}})
			meter := adminUsageMeterDTO(r, billingservice.UsageMeterDTO{Key: "requests"})
			page := adminUsageMeterPageDTO(r, []adminUsageMeterResponse{}, 0, 50, 0)
			require.Equal(t, "database", meter.ConfigurationSource)
			require.Equal(t, "database", page.ConfigurationSource)
			require.Equal(t, allow, meter.WritesAllowed)
			require.Equal(t, allow, page.WritesAllowed)
		}
	}
	r := httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), nil)
	source, allow := adminCatalogOwnership(r)
	require.Equal(t, "database", source)
	require.False(t, allow)
}

func TestWriteMeteringError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "meter missing", err: billingservice.ErrUsageMeterNotFound, wantStatus: http.StatusNotFound, wantCode: "usage_meter_not_found"},
		{name: "default missing", err: billingservice.ErrDefaultRateCardNotFound, wantStatus: http.StatusNotFound, wantCode: "default_rate_card_not_found"},
		{name: "product missing", err: billingservice.ErrRateCardProductNotFound, wantStatus: http.StatusNotFound, wantCode: "rate_card_product_not_found"},
		{name: "allowance meter missing", err: billingservice.ErrAllowanceMeterNotFound, wantStatus: http.StatusNotFound, wantCode: "allowance_meter_not_found"},
		{name: "rate card invalid", err: billingservice.ErrUsageRateCardInvalid, wantStatus: http.StatusBadRequest, wantCode: "usage_rate_card_invalid"},
		{name: "meter in use", err: billingservice.ErrMeterInUse, wantStatus: http.StatusConflict, wantCode: "meter_in_use"},
		{name: "meter contract conflict", err: billingservice.ErrMeterRateCardConflict, wantStatus: http.StatusConflict, wantCode: "meter_rate_card_conflict"},
		{name: "allowance source invalid", err: billingservice.ErrAllowanceSourceInvalid, wantStatus: http.StatusConflict, wantCode: "allowance_source_invalid"},
		{name: "allowance source in use", err: billingservice.ErrAllowanceSourceInUse, wantStatus: http.StatusConflict, wantCode: "allowance_source_in_use"},
		{name: "default required", err: billingservice.ErrDefaultRateCardRequired, wantStatus: http.StatusConflict, wantCode: "default_rate_card_required"},
		{name: "overrides exist", err: billingservice.ErrRateCardHasOverrides, wantStatus: http.StatusConflict, wantCode: "rate_card_has_overrides"},
		{name: "currency mismatch", err: billingservice.ErrRateCardCurrencyMismatch, wantStatus: http.StatusConflict, wantCode: "rate_card_currency_mismatch"},
		{name: "wrapped sentinel", err: errors.Join(errors.New("context"), billingservice.ErrMeterInUse), wantStatus: http.StatusConflict, wantCode: "meter_in_use"},
		{name: "unexpected", err: errors.New("sqlstate secret"), wantStatus: http.StatusInternalServerError, wantCode: api.CodeInternalError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			r := httprequest.NewHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil), nil)
			writeMeteringError(r, test.err)

			require.Equal(t, test.wantStatus, recorder.Code)
			var response api.ErrorResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			require.Equal(t, test.wantCode, response.Error.Code)
			if test.wantStatus == http.StatusInternalServerError {
				require.NotContains(t, recorder.Body.String(), "sqlstate secret")
			}
		})
	}
}
