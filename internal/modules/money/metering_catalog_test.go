package money

import (
	"testing"

	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

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
