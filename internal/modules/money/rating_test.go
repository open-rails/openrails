package money

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
)

// Every usage price the catalog accepts settles: the probe settlement runs
// before writing prices each model, a dynamic card at its stated amount.
func TestSettlementPricesEveryAcceptedUsageModel(t *testing.T) {
	ten := int64(10)
	stated := int64(450)
	for name, test := range map[string]struct {
		price catalog.RatePrice
		cell  string
		want  int64
	}{
		"per_unit":  {price: catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 3}}, want: 12},
		"matrix":    {price: catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{Matrix: &catalog.Matrix{Dimension: "model", Cells: map[string]catalog.MatrixCell{"large": {UnitAmount: 5}}}}}, cell: "large", want: 20},
		"graduated": {price: catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{Mode: catalog.TierModeGraduated, Tiers: []catalog.RateTier{{UpTo: &ten, UnitAmount: 2}, {UnitAmount: 1}}}}, want: 8},
		"package":   {price: catalog.RatePrice{Model: catalog.ModelPackage, Package: &catalog.PackagePrice{Amount: 7, PackageSize: 3}}, want: 14},
		"dynamic":   {price: catalog.RatePrice{Model: catalog.ModelDynamic}, want: stated},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, catalogrules.ValidateUsagePrice(name, &test.price))
			u := usageQuantity{card: ratingCard{MeterKey: "work", Aggregation: catalog.AggregationSum, Price: test.price}, cell: test.cell, quantity: 4, stated: &stated}
			require.NoError(t, u.priceable())
			got, err := u.price(0)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
	volume := catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{Mode: catalog.TierModeVolume, Tiers: []catalog.RateTier{{UnitAmount: 1}}}}
	require.Error(t, catalogrules.ValidateUsagePrice("volume", &volume))
	require.ErrorIs(t, usageQuantity{card: ratingCard{MeterKey: "work", Price: volume}, quantity: 4}.priceable(), errVolumeTiers)
}
