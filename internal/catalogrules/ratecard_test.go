package catalogrules

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
)

func TestRateUsage(t *testing.T) {
	runtime := catalog.RateCard{Meter: "droplet-runtime", PaymentTerm: " IN_ARREARS ", Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "usd", PerUnit: &catalog.PerUnitPrice{
		DivideBy: 3_600, Round: catalog.RoundUp,
		Matrix: &catalog.Matrix{Dimension: "size_slug", Cells: map[string]catalog.MatrixCell{"s-1vcpu-1gb": {UnitAmount: 8_930, MaximumAmount: 6_000_000}}},
	}}}
	require.NoError(t, ValidateRateCard("runtime", &runtime))
	require.Equal(t, catalog.PaymentInArrears, runtime.PaymentTerm)
	egress := catalog.RateCard{Meter: "public-egress", Allowance: &catalog.Allowance{AccrueFrom: "droplet-runtime", Cap: "28d"},
		Price: catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 10_000, DivideBy: 1_073_741_824, Round: catalog.RoundUp}}}
	require.NoError(t, ValidateRateCard("egress", &egress))

	for _, tc := range []struct {
		name string
		card catalog.RateCard
		dim  string
		qty  int64
		want int64
	}{
		{"a full month hits the cell cap", runtime, "s-1vcpu-1gb", 720 * 3_600, 6_000_000},
		{"30s bills its prorated cost", runtime, "s-1vcpu-1gb", 30, 75},
		{"zero usage is free", runtime, "s-1vcpu-1gb", 0, 0},
		{"a base card ignores the dimension", egress, "ignored", 5 * 1_073_741_824, 50_000},
		{"a partial GiB rounds up", egress, "", 1, 1},
	} {
		got, err := RateUsage(tc.card, tc.dim, tc.qty)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
	_, err := RateUsage(runtime, "s-99vcpu", 3_600)
	require.ErrorContains(t, err, "no matrix cell", "an unknown cell is an error, not zero")
}

func TestValidateRateCardRefusals(t *testing.T) {
	perUnit := catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "usd", PerUnit: &catalog.PerUnitPrice{UnitAmount: 1}}
	flat := catalog.RatePrice{Model: catalog.ModelFlat, Currency: "usd", Flat: &catalog.FlatPrice{Amount: 1}}
	for want, card := range map[string]catalog.RateCard{
		"requires a meter":           {Price: perUnit},
		"must not reference a meter": {Meter: "m", Price: flat},
		"cannot have an allowance":   {Price: flat, Allowance: &catalog.Allowance{Included: 1}},
		"payment_term must be":       {Meter: "m", PaymentTerm: "monthly", Price: perUnit},
	} {
		require.ErrorContains(t, ValidateRateCard("card", &card), want)
	}
}
