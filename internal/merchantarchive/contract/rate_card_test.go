package contract

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

func TestRateCardArchiveMatchesPersistedPrice(t *testing.T) {
	ceiling := int64(10)
	for _, price := range []pricing.RatePrice{
		{Model: pricing.ModelFlat, Currency: "USD", Flat: &pricing.FlatPrice{Amount: math.MaxInt64}},
		{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{UnitAmount: 9007199254740993, DivideBy: 60, MaximumAmount: math.MaxInt64}},
		{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{DivideBy: 60, Round: pricing.RoundUp, MaximumAmount: math.MaxInt64, Matrix: &pricing.Matrix{Dimension: "size", Cells: map[string]pricing.MatrixCell{"small": {UnitAmount: 0}, "large": {UnitAmount: 9007199254740993, MaximumAmount: math.MaxInt64, Included: 10}}}}},
		{Model: pricing.ModelTiered, Currency: "USD", Tiered: &pricing.TieredPrice{Mode: pricing.TierModeGraduated, Tiers: []pricing.RateTier{{UpTo: &ceiling, UnitAmount: 100, FlatAmount: 9007199254740993}, {UpTo: nil, UnitAmount: 50}}}},
		{Model: pricing.ModelPackage, Currency: "USD", Package: &pricing.PackagePrice{Amount: 9007199254740993, PackageSize: 100, FreeUnits: 10}},
	} {
		raw, err := json.Marshal(price)
		require.NoError(t, err)
		require.NoError(t, validateJSON("catalog_rate_cards.price", string(raw)), string(raw))
	}
	for _, raw := range []string{
		`{"flat":{"amount":2}}`,
		`{"per_unit":{"unit_amount":2}}`,
		`{"per_unit":{"maximum_amount":2}}`,
		`{"per_unit":{"matrix":{"dimension":"size","cells":{"large":{"unit_amount":2}}}}}`,
		`{"tiered":{"tiers":[{"unit_amount":2,"flat_amount":"1"}]}}`,
		`{"package":{"amount":2}}`,
		`{"flat":{"amount":"9223372036854775808"}}`,
		`{"flat":{"amount":"1e3"}}`,
		`{"flat":{"amount":"+1"}}`,
		`{"per_unit":{"unit_amount":"2","security_key":"secret"}}`,
		`{"per_unit":{"matrix":{"dimension":"size","cells":{"large":{"unit_amount":"2","card_number":"4111111111111111"}}}}}`,
		`{"maximum_amount":"2"}`,
		`{"matrix":{"dimension":"size","cells":{"large":{"unit_amount":"2"}}}}`,
	} {
		require.Error(t, validateJSON("catalog_rate_cards.price", raw), raw)
	}
}
