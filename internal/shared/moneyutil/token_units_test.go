package moneyutil

import (
	"math"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestTokenNativeUnitsRoundTripWithoutFiatScaling(t *testing.T) {
	for _, code := range []string{"SOL", "USDC"} {
		cur, ok := LookupCurrency(code)
		require.True(t, ok)
		require.Equal(t, "crypto", cur.Kind)
		wire, ok := billing.LookupCurrency(code)
		require.True(t, ok)
		require.Equal(t, cur.Decimals, wire.Decimals)
		require.Equal(t, 0, wire.NativeShift())
		for _, native := range []int64{0, 1, 1_000_000, 1_000_000_000, math.MaxInt64, math.MinInt64} {
			minor, err := NativeToRailMinorExact(code, native)
			require.NoError(t, err)
			require.Equal(t, native, int64(minor))
			back, err := RailMinorToNative(code, minor)
			require.NoError(t, err)
			require.Equal(t, native, back)
		}
	}
}
