//go:build greenfield

package greenfield_test

import (
	"math"
	"testing"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
)

func TestExactIntegerMoneyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  moneyutil.Cents
	}{
		{"1.004", 100},
		{"1.005", 101},
		{"-1.005", -101},
		{"90071992547409.93", 9007199254740993},
	} {
		got, err := moneyutil.ParseDecimalToCents(tc.input)
		require.NoError(t, err, tc.input)
		require.Equal(t, tc.want, got, tc.input)
	}
	_, err := moneyutil.ParseDecimalToCents("9223372036854775808")
	require.Error(t, err, "decimal overflow must not wrap an int64")

	// USD uses six internal decimal places and two provider minor places.
	exact, err := moneyutil.NativeToRailMinorExact("usd", 1_000_000)
	require.NoError(t, err)
	require.Equal(t, moneyutil.Cents(100), exact)
	ceil, err := moneyutil.NativeToRailMinor("USD", 1_000_001)
	require.NoError(t, err)
	require.Equal(t, moneyutil.Cents(101), ceil)
	_, err = moneyutil.NativeToRailMinorExact("USD", 1_000_001)
	require.Error(t, err, "price conversion must reject sub-cent terms")

	// Zero-decimal JPY has a different registered scale and must not reuse USD's
	// hard-coded divide-by-10,000 conversion.
	yen, err := moneyutil.NativeToRailMinorExact("JPY", 10_000)
	require.NoError(t, err)
	require.Equal(t, moneyutil.Cents(1), yen)
	wide, err := moneyutil.RailMinorToNative("JPY", moneyutil.Cents(2))
	require.NoError(t, err)
	require.Equal(t, int64(20_000), wide)

	_, err = moneyutil.NativeToRailMinorExact("UNKNOWN", 1)
	require.Error(t, err, "unknown currencies must fail closed")
	_, err = moneyutil.RailMinorToNative("USD", moneyutil.Cents(math.MaxInt64))
	require.Error(t, err, "native widening must reject int64 overflow")
}
