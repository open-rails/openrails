package catalogrules

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
)

func TestValidateRateCardRefusals(t *testing.T) {
	perUnit := catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "usd", PerUnit: &catalog.PerUnitPrice{UnitAmount: 1}}
	flat := catalog.RatePrice{Model: catalog.ModelFlat, Currency: "usd", Flat: &catalog.FlatPrice{Amount: 1}}
	volume := catalog.RatePrice{Model: catalog.ModelTiered, Currency: "usd", Tiered: &catalog.TieredPrice{Mode: catalog.TierModeVolume, Tiers: []catalog.RateTier{{UnitAmount: 1}}}}
	for want, card := range map[string]catalog.RateCard{
		"requires a meter":           {Price: perUnit},
		"must not reference a meter": {Meter: "m", Price: flat},
		"cannot have an allowance":   {Price: flat, Allowance: &catalog.Allowance{Included: 1}},
		"payment_term must be":       {Meter: "m", PaymentTerm: "monthly", Price: perUnit},
		"states no payment_term":     {Meter: "m", PaymentTerm: catalog.PaymentInArrears, Price: perUnit},
		"cannot use volume tiers":    {Meter: "m", Price: volume},
	} {
		require.ErrorContains(t, ValidateRateCard("card", &card), want)
	}
}
