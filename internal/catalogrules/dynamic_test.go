package catalogrules

import (
	"testing"

	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestDynamicPriceRequiresSumAndNoPriceBlock(t *testing.T) {
	price := catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "USD"}
	require.NoError(t, ValidateUsagePrice("price", &price))
	require.NoError(t, ValidateMeterPrice("meter", catalog.AggregationSum, price))
	require.ErrorContains(t, ValidateMeterPrice("meter", catalog.AggregationCount, price), "requires a sum meter")
	price.PerUnit = &catalog.PerUnitPrice{UnitAmount: 1}
	require.ErrorContains(t, ValidateUsagePrice("price", &price), "no price block")
}
