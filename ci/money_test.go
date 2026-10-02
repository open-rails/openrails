//go:build greenfield

package greenfield_test

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"github.com/open-rails/openrails"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
)

func TestExactIntegerMoneyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  moneyutil.Cents
	}{
		{"0.004999999999999999", 0},
		{"0.005", 1},
		{"-0.005", -1},
		{"1.004", 100},
		{"1.005", 101},
		{"-1.005", -101},
		{"90071992547409.93", 9007199254740993},
		{"92233720368547758.074", math.MaxInt64},
		{"-92233720368547758.08", math.MinInt64},
		{"-92233720368547758.075", math.MinInt64},
		{"-92233720368547758.084", math.MinInt64},
	} {
		got, err := moneyutil.ParseDecimalToCents(tc.input)
		require.NoError(t, err, tc.input)
		require.Equal(t, tc.want, got, tc.input)
	}
	for _, input := range []string{"", "NaN", "Inf", "1.2.3", "92233720368547758.075", "-92233720368547758.085", "9223372036854775808"} {
		_, err := moneyutil.ParseDecimalToCents(input)
		require.Error(t, err, input)
	}

	// USD uses six internal decimal places and two provider minor places.
	exact, err := moneyutil.NativeToRailMinorExact("usd", 1_000_000)
	require.NoError(t, err)
	require.Equal(t, moneyutil.Cents(100), exact)
	ceil, err := moneyutil.NativeToRailMinor("USD", 1_000_001)
	require.NoError(t, err)
	require.Equal(t, moneyutil.Cents(101), ceil)
	_, err = moneyutil.NativeToRailMinorExact("USD", 1_000_001)
	require.Error(t, err, "price conversion must reject sub-cent terms")

	// JPY has four internal decimal places and zero provider decimal places.
	yen, err := moneyutil.NativeToRailMinorExact("JPY", 10_000)
	require.NoError(t, err)
	require.Equal(t, moneyutil.Cents(1), yen)
	wide, err := moneyutil.RailMinorToNative("JPY", moneyutil.Cents(2))
	require.NoError(t, err)
	require.Equal(t, int64(20_000), wide)

	_, err = moneyutil.NativeToRailMinorExact("UNKNOWN", 1)
	require.Error(t, err, "unknown currencies must fail closed")
	for _, minor := range []moneyutil.Cents{922337203685478, -922337203685478} {
		_, err = moneyutil.RailMinorToNative("USD", minor)
		require.Error(t, err, "native widening must reject positive and negative int64 overflow")
	}
	for _, minor := range []moneyutil.Cents{922337203685477, -922337203685477} {
		native, err := moneyutil.RailMinorToNative("USD", minor)
		require.NoError(t, err)
		back, err := moneyutil.NativeToRailMinorExact("USD", native)
		require.NoError(t, err)
		require.Equal(t, minor, back)
	}
	maximum, err := moneyutil.NativeToRailMinor("USD", math.MaxInt64)
	require.NoError(t, err)
	require.Equal(t, moneyutil.Cents(922337203685478), maximum, "ceiling must not overflow while adding the rounding remainder")
	require.Equal(t, "92233720368547758.07", moneyutil.FormatCentsDecimal(math.MaxInt64))
	require.Equal(t, "-92233720368547758.08", moneyutil.FormatCentsDecimal(math.MinInt64))
}

// These are the public request and response DTOs; a JSON consumer must receive
// decimal strings, because a JSON number would lose odd integers above 2^53.
func TestMoneyJSONPreservesInt64(t *testing.T) {
	for _, amount := range []int64{math.MinInt64, -9007199254740993, 0, 9007199254740993, math.MaxInt64} {
		t.Run(strconv.FormatInt(amount, 10), func(t *testing.T) {
			request := openrails.PriceCreateParams{UnitAmount: amount}
			raw, err := json.Marshal(request)
			require.NoError(t, err)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(raw, &wire))
			require.Equal(t, strconv.FormatInt(amount, 10), wire["unit_amount"])
			var read openrails.PriceCreateParams
			require.NoError(t, json.Unmarshal(raw, &read))
			require.Equal(t, amount, read.UnitAmount)

			response := openrails.Payment{Amount: amount, AmountRefunded: amount, Price: &openrails.PublicPrice{UnitAmount: amount}}
			raw, err = json.Marshal(response)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &wire))
			require.Equal(t, strconv.FormatInt(amount, 10), wire["amount"])
			require.Equal(t, strconv.FormatInt(amount, 10), wire["amount_refunded"])
			require.Equal(t, strconv.FormatInt(amount, 10), wire["price"].(map[string]any)["unit_amount"])
			var payment openrails.Payment
			require.NoError(t, json.Unmarshal(raw, &payment))
			require.Equal(t, amount, payment.Amount)
			require.Equal(t, amount, payment.AmountRefunded)
			require.Equal(t, amount, payment.Price.UnitAmount)
		})
	}
	for _, value := range []string{`9007199254740993`, `"1.5"`, `"1e3"`, `"9223372036854775808"`, `"-9223372036854775809"`} {
		var request openrails.PriceCreateParams
		require.Error(t, json.Unmarshal([]byte(`{"unit_amount":`+value+`}`), &request), value)
	}
}
