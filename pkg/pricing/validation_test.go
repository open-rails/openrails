package pricing_test

import (
	"testing"

	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateMeter(t *testing.T) {
	tests := []struct {
		name      string
		meter     pricing.Meter
		wantKey   string
		wantError string
	}{
		{
			name: "normalizes a sum meter",
			meter: pricing.Meter{
				Key:           " API Calls ",
				EventType:     " api.calls ",
				ValueProperty: " $.count ",
				Aggregation:   " SUM ",
				GroupBy:       map[string]string{" region ": " $.region "},
			},
			wantKey: "api-calls",
		},
		{
			name: "accepts a count meter without a value property",
			meter: pricing.Meter{
				Key:         "requests",
				Aggregation: pricing.AggregationCount,
			},
			wantKey: "requests",
		},
		{
			name: "rejects sum without a value property",
			meter: pricing.Meter{
				Key:         "requests",
				Aggregation: pricing.AggregationSum,
			},
			wantError: "aggregation sum requires value_property",
		},
		{
			name: "rejects a count value property",
			meter: pricing.Meter{
				Key:           "requests",
				Aggregation:   pricing.AggregationCount,
				ValueProperty: "$.count",
			},
			wantError: "aggregation count must not set value_property",
		},
		{
			name: "rejects a blank group property",
			meter: pricing.Meter{
				Key:         "requests",
				Aggregation: pricing.AggregationCount,
				GroupBy:     map[string]string{"region": " "},
			},
			wantError: "group_by dimensions and properties must be non-empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := pricing.ValidateMeter("meter", &tt.meter)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKey, tt.meter.Key)
			if tt.name == "normalizes a sum meter" {
				assert.Equal(t, map[string]string{"region": "$.region"}, tt.meter.GroupBy)
			}
		})
	}
}

func TestValidateUsagePrice(t *testing.T) {
	unbounded := int64(0)
	tests := []struct {
		name      string
		price     pricing.RatePrice
		wantError string
	}{
		{
			name: "per unit",
			price: pricing.RatePrice{
				Model:    pricing.ModelPerUnit,
				Currency: "usd",
				PerUnit:  &pricing.PerUnitPrice{UnitAmount: 1_000_000, DivideBy: 100},
			},
		},
		{
			name: "tiered graduated",
			price: pricing.RatePrice{
				Model:    pricing.ModelTiered,
				Currency: "eur",
				Tiered: &pricing.TieredPrice{
					Mode: pricing.TierModeGraduated,
					Tiers: []pricing.RateTier{
						{UpTo: int64Pointer(10), UnitAmount: 500_000},
						{UpTo: nil, UnitAmount: 250_000},
					},
				},
			},
		},
		{
			name: "package",
			price: pricing.RatePrice{
				Model:    pricing.ModelPackage,
				Currency: "jpy",
				Package:  &pricing.PackagePrice{PackageSize: 100, Amount: 10_000},
			},
		},
		{
			name: "flat is not usage pricing",
			price: pricing.RatePrice{
				Model:    pricing.ModelFlat,
				Currency: "usd",
				Flat:     &pricing.FlatPrice{Amount: 1_000_000},
			},
			wantError: "flat prices cannot be attached to a meter",
		},
		{
			name: "model block mismatch",
			price: pricing.RatePrice{
				Model:    pricing.ModelPerUnit,
				Currency: "usd",
				Package:  &pricing.PackagePrice{PackageSize: 100, Amount: 10_000},
			},
			wantError: "model per_unit requires per_unit block",
		},
		{
			name: "bounded final tier",
			price: pricing.RatePrice{
				Model:    pricing.ModelTiered,
				Currency: "usd",
				Tiered: &pricing.TieredPrice{
					Mode:  pricing.TierModeVolume,
					Tiers: []pricing.RateTier{{UpTo: &unbounded, UnitAmount: 1}},
				},
			},
			wantError: "last tier must be unbounded",
		},
		{
			name: "invalid currency",
			price: pricing.RatePrice{
				Model:    pricing.ModelPackage,
				Currency: "USDC",
				Package:  &pricing.PackagePrice{PackageSize: 100, Amount: 10_000},
			},
			wantError: "currency must be an ISO money currency",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := pricing.ValidateUsagePrice("rate card", &tt.price)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 3, len(tt.price.Currency))
		})
	}
}

func TestValidateDimensions(t *testing.T) {
	price := pricing.RatePrice{
		Model: pricing.ModelPerUnit,
		PerUnit: &pricing.PerUnitPrice{
			Matrix: &pricing.Matrix{
				Dimension: "size",
				Cells:     map[string]pricing.MatrixCell{"small": {UnitAmount: 1}},
			},
		},
	}
	require.NoError(t, pricing.ValidateDimensions(
		"rate card",
		map[string]string{"size": "$.size", "region": "$.region"},
		map[string][]string{"region": {"eu"}},
		&price,
	))
	require.ErrorContains(t, pricing.ValidateDimensions(
		"rate card",
		map[string]string{"region": "$.region"},
		nil,
		&price,
	), "matrix dimension")
	require.ErrorContains(t, pricing.ValidateDimensions(
		"rate card",
		nil,
		map[string][]string{"region": {"eu"}},
		nil,
	), "filter key")
}

func TestValidateFilter(t *testing.T) {
	filter := map[string][]string{" region ": {" eu ", "eu", "us"}}
	require.NoError(t, pricing.ValidateFilter("rate card", &filter))
	assert.Equal(t, map[string][]string{"region": {"eu", "us"}}, filter)

	invalid := map[string][]string{"region": {}}
	require.ErrorContains(t, pricing.ValidateFilter("rate card", &invalid), "at least one value")
}

func int64Pointer(value int64) *int64 {
	return &value
}
