package money

import (
	"math"
	"testing"

	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

func TestNormalizeMeteringPageBounds(t *testing.T) {
	tests := []struct {
		name         string
		limit        int
		offset       int
		wantLimit    int
		wantOffset   int
		wantLimit32  int32
		wantOffset32 int32
	}{
		{
			name: "defaults and floors", limit: 0, offset: -1,
			wantLimit: defaultMeteringPageSize, wantOffset: 0,
			wantLimit32: defaultMeteringPageSize, wantOffset32: 0,
		},
		{
			name: "caps page size", limit: maxMeteringPageSize + 1, offset: 1,
			wantLimit: maxMeteringPageSize, wantOffset: 1,
			wantLimit32: maxMeteringPageSize, wantOffset32: 1,
		},
		{
			name: "caps SQL offset", limit: 1, offset: math.MaxInt,
			wantLimit: 1, wantOffset: math.MaxInt32,
			wantLimit32: 1, wantOffset32: math.MaxInt32,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit, offset := normalizeMeteringPage(test.limit, test.offset)
			require.Equal(t, test.wantLimit, limit)
			require.Equal(t, test.wantOffset, offset)
			require.Equal(t, test.wantLimit32, meteringPageInt32(limit))
			require.Equal(t, test.wantOffset32, meteringPageInt32(offset))
		})
	}
}

func TestValidateAllowanceSourcePrice(t *testing.T) {
	meter := pricing.Meter{
		Key:         "runtime",
		Aggregation: pricing.AggregationSum,
		GroupBy: map[string]string{
			"size":        "metadata.size",
			"resource_id": "metadata.resource_id",
		},
	}
	valid := pricing.RatePrice{
		Model:    pricing.ModelPerUnit,
		Currency: "USD",
		PerUnit: &pricing.PerUnitPrice{Matrix: &pricing.Matrix{
			Dimension: "size",
			Cells: map[string]pricing.MatrixCell{
				"small": {UnitAmount: 10_000, Included: 100},
			},
		}},
	}
	require.NoError(t, validateAllowanceSourcePrice(meter, valid, "USD"))

	tests := []struct {
		name  string
		meter pricing.Meter
		price pricing.RatePrice
	}{
		{name: "currency mismatch", meter: meter, price: func() pricing.RatePrice {
			price := valid
			price.Currency = "EUR"
			return price
		}()},
		{name: "non-matrix price", meter: meter, price: pricing.RatePrice{
			Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{UnitAmount: 10_000},
		}},
		{name: "missing resource dimension", meter: pricing.Meter{
			Key: "runtime", Aggregation: pricing.AggregationSum,
			GroupBy: map[string]string{"size": "metadata.size"},
		}, price: valid},
		{name: "no included cells", meter: meter, price: func() pricing.RatePrice {
			price := valid
			price.PerUnit = &pricing.PerUnitPrice{Matrix: &pricing.Matrix{
				Dimension: "size",
				Cells:     map[string]pricing.MatrixCell{"small": {UnitAmount: 10_000}},
			}}
			return price
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAllowanceSourcePrice(test.meter, test.price, "USD")
			require.ErrorIs(t, err, ErrAllowanceSourceInvalid)
		})
	}
}
