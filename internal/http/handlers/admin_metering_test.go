package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/pricing"
	billingservice "github.com/open-rails/openrails/pkg/service"
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
		ProductID: productID,
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
		ProductID: productID,
		Filter:    map[string][]string{},
		Price: pricing.RatePrice{
			Model:    pricing.ModelPerUnit,
			Currency: "GBP",
			PerUnit:  &pricing.PerUnitPrice{UnitAmount: 100},
		},
	})
	require.EqualError(t, err, `money: unknown currency "GBP"`)
}

func TestAdminUsageMeterDTOOwnership(t *testing.T) {
	meter := billingservice.UsageMeterDTO{Key: "requests"}

	manifest := httprequest.NewHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil),
		&app.Runtime{Config: &config.Config{MerchantSource: config.MerchantSourceManifest}},
	)
	manifestDTO := adminUsageMeterDTO(manifest, meter)
	require.Equal(t, config.MerchantSourceManifest, manifestDTO.ConfigurationSource)
	require.False(t, manifestDTO.WritesAllowed)

	apiDriven := httprequest.NewHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil),
		&app.Runtime{Config: &config.Config{MerchantSource: config.MerchantSourceAPI}},
	)
	apiDTO := adminUsageMeterDTO(apiDriven, meter)
	require.Equal(t, config.MerchantSourceAPI, apiDTO.ConfigurationSource)
	require.True(t, apiDTO.WritesAllowed)
}

func TestAdminUsageMeterPageDTOOwnershipWithoutItems(t *testing.T) {
	tests := []struct {
		name          string
		source        string
		writesAllowed bool
	}{
		{
			name:          "api catalog stays writable",
			source:        config.MerchantSourceAPI,
			writesAllowed: true,
		},
		{
			name:          "manifest catalog stays read-only",
			source:        config.MerchantSourceManifest,
			writesAllowed: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httprequest.NewHTTP(
				httptest.NewRecorder(),
				httptest.NewRequest(http.MethodGet, "/", nil),
				&app.Runtime{Config: &config.Config{MerchantSource: test.source}},
			)
			page := adminUsageMeterPageDTO(
				r,
				[]adminUsageMeterResponse{},
				0,
				50,
				0,
			)

			require.Empty(t, page.Items)
			require.Equal(t, test.source, page.ConfigurationSource)
			require.Equal(t, test.writesAllowed, page.WritesAllowed)
		})
	}
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
		{name: "meter in use", err: billingservice.ErrMeterInUse, wantStatus: http.StatusConflict, wantCode: "meter_in_use"},
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
