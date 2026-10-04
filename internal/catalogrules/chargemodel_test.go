package catalogrules

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
)

func i64(v int64) *int64 { return &v }

// $0.0100/credit to 2k, $0.0090 to 10k, $0.0080 beyond.
func threeTiers() []ChargeTier {
	return []ChargeTier{{UpTo: i64(2_000), UnitAmount: 10_000}, {UpTo: i64(10_000), UnitAmount: 9_000}, {UnitAmount: 8_000}}
}

func TestChargeModelRate(t *testing.T) {
	perUnit := func(unit, divide int64, round catalog.Round) ChargeModel {
		return ChargeModel{Kind: catalog.ModelPerUnit, UnitAmount: unit, DivideBy: divide, Round: round}
	}
	droplet := perUnit(5_950, 3_600, "") // $0.00595/h billed per second
	capped := droplet
	capped.MaximumAmount = 4_000_000
	volume := ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeVolume, Tiers: threeTiers()}
	graduated := ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeGraduated, Tiers: threeTiers()}
	gradFlat := ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeGraduated, Tiers: []ChargeTier{
		{UpTo: i64(1_000), UnitAmount: 100, FlatAmount: 50_000}, {UnitAmount: 50},
	}}
	pkg := ChargeModel{Kind: catalog.ModelPackage, PackageSize: 100, PackageAmount: 5_000_000, FreeUnits: 100}

	for _, tt := range []struct {
		name string
		cm   ChargeModel
		qty  int64
		want int64
	}{
		{"per_unit pro-rates exactly", droplet, 5_400, 8_925},
		{"per_unit zero quantity", droplet, 0, 0},
		{"divide_by 0 means 1", perUnit(7, 0, ""), 3, 21},
		{"half_up 6.67", perUnit(10, 3, catalog.RoundHalfUp), 2, 7},
		{"up 6.67", perUnit(10, 3, catalog.RoundUp), 2, 7},
		{"down 6.67", perUnit(10, 3, catalog.RoundDown), 2, 6},
		{"up 3.33", perUnit(10, 3, catalog.RoundUp), 1, 4},
		{"half_up 3.33", perUnit(10, 3, catalog.RoundHalfUp), 1, 3},
		{"default rounds exact half up", perUnit(1, 2, ""), 1, 1},
		{"down exact half", perUnit(1, 2, catalog.RoundDown), 1, 0},
		{"cap applies", capped, 3_000 * 3_600, 4_000_000},
		{"no per-line floor", perUnit(5_950, 3_600, catalog.RoundUp), 30, 50},
		{"volume whole qty at landed band", volume, 5_000, 45_000_000},
		{"volume at band ceiling", volume, 2_000, 20_000_000},
		{"volume cliff past ceiling", volume, 2_001, 18_009_000},
		{"graduated slices bands", graduated, 5_000, 47_000_000},
		{"graduated partial band", graduated, 5_333, 49_997_000},
		{"graduated into unbounded band", graduated, 10_001, 20_000_000 + 72_000_000 + 8_000},
		{"graduated flat within first band", gradFlat, 500, 100_000},
		{"graduated flat once per reached band", gradFlat, 1_500, 175_000},
		{"package rounds blocks up after free units", pkg, 201, 10_000_000},
		{"package all free", pkg, 100, 0},
		{"flat ignores quantity", ChargeModel{Kind: catalog.ModelFlat, FlatAmount: 5_000_000}, 99, 5_000_000},
		{"overflow-safe intermediate", perUnit(math.MaxInt64, math.MaxInt64, ""), 3, 3},
	} {
		got, err := tt.cm.Rate(tt.qty)
		require.NoError(t, err, tt.name)
		require.Equal(t, tt.want, got, tt.name)
	}

	for name, tt := range map[string]struct {
		cm  ChargeModel
		qty int64
	}{
		"negative quantity":     {droplet, -1},
		"unknown kind":          {ChargeModel{Kind: "bogus"}, 1},
		"unknown round mode":    {perUnit(10, 3, "banker"), 1},
		"negative unit amount":  {perUnit(-1, 1, ""), 1},
		"unknown tier mode":     {ChargeModel{Kind: catalog.ModelTiered, Mode: "stairs", Tiers: threeTiers()}, 1},
		"no tiers":              {ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeVolume}, 1},
		"volume above top tier": {ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeVolume, Tiers: threeTiers()[:1]}, 2_001},
		"graduated above top":   {ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeGraduated, Tiers: threeTiers()[:1]}, 2_001},
		"zero package size":     {ChargeModel{Kind: catalog.ModelPackage}, 1},
		"per_unit overflow":     {perUnit(math.MaxInt64, 1, ""), 2},
		"graduated overflow":    {ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeGraduated, Tiers: []ChargeTier{{UnitAmount: math.MaxInt64}}}, 2},
		"volume overflow":       {ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeVolume, Tiers: []ChargeTier{{UnitAmount: math.MaxInt64}}}, 2},
		"package overflow":      {ChargeModel{Kind: catalog.ModelPackage, PackageSize: 1, PackageAmount: math.MaxInt64}, 2},
	} {
		_, err := tt.cm.Rate(tt.qty)
		require.Error(t, err, name)
	}
}

// Cumulative band charges rely on these models never decreasing in quantity,
// and on the cap bounding every quantity.
func TestChargeModelMonotonicAndCapped(t *testing.T) {
	cappedGrad := ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeGraduated, Tiers: threeTiers(), MaximumAmount: 60_000_000}
	for name, cm := range map[string]ChargeModel{
		"per_unit":  {Kind: catalog.ModelPerUnit, UnitAmount: 5_950, DivideBy: 3_600, Round: catalog.RoundUp},
		"graduated": {Kind: catalog.ModelTiered, Mode: catalog.TierModeGraduated, Tiers: threeTiers()},
		"package":   {Kind: catalog.ModelPackage, PackageSize: 7, PackageAmount: 3, FreeUnits: 5},
		"capped":    cappedGrad,
	} {
		prev := int64(0)
		for q := int64(0); q <= 12_000; q += 37 {
			got, err := cm.Rate(q)
			require.NoError(t, err)
			require.GreaterOrEqual(t, got, prev, "%s at %d", name, q)
			if cm.MaximumAmount > 0 {
				require.LessOrEqual(t, got, cm.MaximumAmount)
			}
			prev = got
		}
	}
}

func TestChargeModelOf(t *testing.T) {
	require.Equal(t,
		ChargeModel{Kind: catalog.ModelPackage, PackageSize: 100, PackageAmount: 5, FreeUnits: 10},
		Of(catalog.RatePrice{Model: catalog.ModelPackage, Package: &catalog.PackagePrice{Amount: 5, PackageSize: 100, FreeUnits: 10}}))
	require.Equal(t,
		ChargeModel{Kind: catalog.ModelFlat, FlatAmount: 9},
		Of(catalog.RatePrice{Model: catalog.ModelFlat, Flat: &catalog.FlatPrice{Amount: 9}}))
	require.Equal(t,
		ChargeModel{Kind: catalog.ModelTiered, Mode: catalog.TierModeVolume, Tiers: []ChargeTier{{UpTo: i64(5), UnitAmount: 2, FlatAmount: 1}, {UnitAmount: 1}}},
		Of(catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{Mode: catalog.TierModeVolume, Tiers: []catalog.RateTier{{UpTo: i64(5), UnitAmount: 2, FlatAmount: 1}, {UnitAmount: 1}}}}))

	matrix := catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{DivideBy: 3_600, Round: catalog.RoundUp, MaximumAmount: 1_000, Matrix: &catalog.Matrix{
		Dimension: "size",
		Cells:     map[string]catalog.MatrixCell{"small": {UnitAmount: 10}, "large": {UnitAmount: 40, MaximumAmount: 4_000}},
	}}}
	small, ok := ForCell(matrix, "small")
	require.True(t, ok)
	require.Equal(t, ChargeModel{Kind: catalog.ModelPerUnit, UnitAmount: 10, DivideBy: 3_600, Round: catalog.RoundUp, MaximumAmount: 1_000}, small, "falls back to the price cap")
	large, _ := ForCell(matrix, "large")
	require.Equal(t, int64(4_000), large.MaximumAmount, "cell cap wins")
	_, ok = ForCell(matrix, "huge")
	require.False(t, ok)
	_, ok = ForCell(catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 1}}, "small")
	require.False(t, ok)
}
