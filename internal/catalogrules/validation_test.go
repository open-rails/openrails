package catalogrules

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
)

func TestValidateMeter(t *testing.T) {
	meter := Meter{Key: " API Calls!", EventType: " api.calls ", ValueProperty: " $.count ", Aggregation: " SUM ",
		GroupBy: map[string]string{" region ": " $.region "}}
	require.NoError(t, ValidateMeter("meter", &meter))
	require.Equal(t, Meter{Key: "api-calls", EventType: "api.calls", ValueProperty: "$.count", Aggregation: catalog.AggregationSum,
		GroupBy: map[string]string{"region": "$.region"}}, meter)

	for want, m := range map[string]Meter{
		"key is required":                                      {Key: " !! ", Aggregation: catalog.AggregationCount},
		"aggregation is required":                              {Key: "requests"},
		"aggregation must be one of":                           {Key: "requests", Aggregation: "avg"},
		"aggregation sum requires value_property":              {Key: "requests", Aggregation: catalog.AggregationSum},
		"aggregation count must not set value_property":        {Key: "requests", Aggregation: catalog.AggregationCount, ValueProperty: "$.count"},
		"group_by dimensions and properties must be non-empty": {Key: "requests", Aggregation: catalog.AggregationCount, GroupBy: map[string]string{"region": " "}},
		"duplicate group_by dimension":                         {Key: "requests", Aggregation: catalog.AggregationCount, GroupBy: map[string]string{"a": "$.a", " a ": "$.b"}},
	} {
		require.ErrorContains(t, ValidateMeter("meter", &m), want)
	}
	require.Error(t, ValidateMeter("meter", nil))
	require.True(t, BillingSupported(" SUM "))
	require.True(t, BillingSupported(catalog.AggregationCount))
	require.False(t, BillingSupported(catalog.AggregationMax))
}

func TestValidateUsagePrice(t *testing.T) {
	perUnit := func(pu catalog.PerUnitPrice) catalog.RatePrice { return catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "usd", PerUnit: &pu} }
	tiered := func(mode catalog.TierMode, tiers ...catalog.RateTier) catalog.RatePrice {
		return catalog.RatePrice{Model: catalog.ModelTiered, Currency: "eur", Tiered: &catalog.TieredPrice{Mode: mode, Tiers: tiers}}
	}
	pkg := func(pp catalog.PackagePrice) catalog.RatePrice { return catalog.RatePrice{Model: catalog.ModelPackage, Currency: "jpy", Package: &pp} }
	matrix := &catalog.Matrix{Dimension: " size ", Cells: map[string]catalog.MatrixCell{"small": {UnitAmount: 1}}}

	for name, price := range map[string]catalog.RatePrice{
		"per unit":         perUnit(catalog.PerUnitPrice{UnitAmount: 1_000_000, DivideBy: 100, Round: " UP "}),
		"per unit matrix":  perUnit(catalog.PerUnitPrice{Matrix: matrix}),
		"graduated tiers":  tiered(" Graduated ", catalog.RateTier{UpTo: i64(10), UnitAmount: 500_000}, catalog.RateTier{UnitAmount: 250_000}),
		"package":          pkg(catalog.PackagePrice{PackageSize: 100, Amount: 10_000}),
		"currency omitted": {Model: catalog.ModelPackage, Package: &catalog.PackagePrice{PackageSize: 1, Amount: 1}},
	} {
		require.NoError(t, ValidateUsagePrice("rate card", &price), name)
	}
	normalized := perUnit(catalog.PerUnitPrice{UnitAmount: 1, Round: " UP ", Matrix: &catalog.Matrix{Dimension: " size ", Cells: map[string]catalog.MatrixCell{"s": {}}}})
	require.NoError(t, ValidateUsagePrice("rate card", &normalized))
	require.Equal(t, "USD", normalized.Currency)
	require.Equal(t, catalog.RoundUp, normalized.PerUnit.Round)
	require.Equal(t, "size", normalized.PerUnit.Matrix.Dimension)

	for want, price := range map[string]catalog.RatePrice{
		"flat prices cannot be attached to a meter": {Model: catalog.ModelFlat, Currency: "usd", Flat: &catalog.FlatPrice{Amount: 1}},
		"model per_unit requires per_unit block":    {Model: catalog.ModelPerUnit, Package: &catalog.PackagePrice{PackageSize: 1, Amount: 1}},
		"exactly one matching sub-block": {Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 1},
			Package: &catalog.PackagePrice{PackageSize: 1, Amount: 1}},
		"model must be one of":                   {Model: "bespoke", PerUnit: &catalog.PerUnitPrice{UnitAmount: 1}},
		"currency must be an ISO money currency": {Model: catalog.ModelPackage, Currency: "USDC", Package: &catalog.PackagePrice{PackageSize: 1, Amount: 1}},
		"round must be up, down or half_up":      perUnit(catalog.PerUnitPrice{UnitAmount: 1, Round: "banker"}),
		"unit_amount must be >= 0":               perUnit(catalog.PerUnitPrice{UnitAmount: -1}),
		"divide_by must be >= 0":                 perUnit(catalog.PerUnitPrice{UnitAmount: 1, DivideBy: -1}),
		"maximum_amount must be >= 0":            perUnit(catalog.PerUnitPrice{UnitAmount: 1, MaximumAmount: -1}),
		"matrix requires a dimension":            perUnit(catalog.PerUnitPrice{Matrix: &catalog.Matrix{Cells: map[string]catalog.MatrixCell{"s": {}}}}),
		"matrix requires at least one cell":      perUnit(catalog.PerUnitPrice{Matrix: &catalog.Matrix{Dimension: "size"}}),
		`matrix cell "s" amounts must be >= 0`:   perUnit(catalog.PerUnitPrice{Matrix: &catalog.Matrix{Dimension: "size", Cells: map[string]catalog.MatrixCell{"s": {Included: -1}}}}),
		"tiered mode must be":                    tiered("stairs", catalog.RateTier{UnitAmount: 1}),
		"tiered price requires tiers":            tiered(catalog.TierModeVolume),
		"the last tier must be unbounded":        tiered(catalog.TierModeVolume, catalog.RateTier{UpTo: i64(0), UnitAmount: 1}),
		"only the last tier may be unbounded":    tiered(catalog.TierModeVolume, catalog.RateTier{UnitAmount: 1}, catalog.RateTier{UnitAmount: 1}),
		"tier up_to values must strictly ascend": tiered(catalog.TierModeVolume, catalog.RateTier{UpTo: i64(5)}, catalog.RateTier{UpTo: i64(5)}, catalog.RateTier{}),
		"tier #1 amounts must be >= 0":           tiered(catalog.TierModeVolume, catalog.RateTier{UnitAmount: -1}),
		"package_size must be > 0":               pkg(catalog.PackagePrice{Amount: 1}),
		"package amount must be > 0":             pkg(catalog.PackagePrice{PackageSize: 1}),
		"free_units must be >= 0":                pkg(catalog.PackagePrice{PackageSize: 1, Amount: 1, FreeUnits: -1}),
	} {
		require.ErrorContains(t, ValidateUsagePrice("rate card", &price), want)
	}
	require.ErrorContains(t, ValidateRatePrice("price", &catalog.RatePrice{Model: catalog.ModelFlat, Flat: &catalog.FlatPrice{}}), "flat price requires a positive amount")
	require.NoError(t, ValidateRatePrice("price", &catalog.RatePrice{Model: catalog.ModelFlat, Flat: &catalog.FlatPrice{Amount: 1}}))
}

func TestValidateFilterAndDimensions(t *testing.T) {
	filter := map[string][]string{" region ": {" eu ", "eu", "us"}}
	require.NoError(t, ValidateFilter("rate card", &filter))
	require.Equal(t, map[string][]string{"region": {"eu", "us"}}, filter)
	for want, f := range map[string]map[string][]string{
		"at least one value":              {"region": {}},
		"filter values must be non-empty": {"region": {" "}},
		"filter keys must be non-empty":   {" ": {"eu"}},
		"duplicate filter key":            {"region": {"eu"}, " region": {"us"}},
	} {
		require.ErrorContains(t, ValidateFilter("rate card", &f), want)
	}

	price := catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{Matrix: &catalog.Matrix{Dimension: "size"}}}
	groupBy := map[string]string{"size": "$.size", "region": "$.region"}
	require.NoError(t, ValidateDimensions("rate card", groupBy, map[string][]string{"region": {"eu"}}, &price))
	require.ErrorContains(t, ValidateDimensions("rate card", map[string]string{"region": "$.region"}, nil, &price), "matrix dimension")
	require.ErrorContains(t, ValidateDimensions("rate card", nil, map[string][]string{"region": {"eu"}}, nil), "filter key")
}

func TestAllowanceAndDurationSpec(t *testing.T) {
	for in, want := range map[string]time.Duration{"1h": time.Hour, " 30D ": 30 * 24 * time.Hour, "2 h": 2 * time.Hour} {
		got, err := ParseDurationSpec(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "0h", "-1d", "1w", "h", "1.5h"} {
		_, err := ParseDurationSpec(in)
		require.Error(t, err, in)
	}

	allowance := catalog.Allowance{Included: 5, AccrueFrom: " Runtime Hours ", Cap: "720h"}
	require.NoError(t, ValidateAllowance("allowance", &allowance))
	require.Equal(t, "runtime-hours", allowance.AccrueFrom)
	require.NoError(t, ValidateAllowance("allowance", nil))
	require.ErrorContains(t, ValidateAllowance("allowance", &catalog.Allowance{Included: -1}), "included must be >= 0")
	require.ErrorContains(t, ValidateAllowance("allowance", &catalog.Allowance{Cap: "1w"}), "allowance.cap")
}
