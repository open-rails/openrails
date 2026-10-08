package purchasedcredits

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProportionalCreditRefund(t *testing.T) {
	for _, tt := range []struct{ face, cash, paid, floor, ceil int64 }{
		{100_000_000, 40_000_000, 80_000_000, 50_000_000, 50_000_000},
		{10, 1, 3, 3, 4},
		{10, 3, 3, 10, 10},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64},
		{math.MaxInt64, 1, math.MaxInt64, 1, 1},
	} {
		require.Equal(t, tt.floor, proportional(tt.face, tt.cash, tt.paid, false))
		require.Equal(t, tt.ceil, proportional(tt.face, tt.cash, tt.paid, true))
	}
}

func TestPartialRecoveryNeverProducesNegativeFundingLeg(t *testing.T) {
	// Two units were withdrawn from two sources by a three-unit cash reversal.
	// Three one-unit recoveries must restore exactly two units, without a
	// negative final leg caused by independently rounding each source.
	var priorFace, priorLive, priorExpired int64
	for cash := int64(1); cash <= 3; cash++ {
		face := proportional(2, cash, 3, false)
		live := recoveredPart(1, 0, face)
		expired := recoveredPart(1, 1, face)
		require.GreaterOrEqual(t, live-priorLive, int64(0))
		require.GreaterOrEqual(t, expired-priorExpired, int64(0))
		require.Equal(t, face-priorFace, live-priorLive+expired-priorExpired)
		priorFace, priorLive, priorExpired = face, live, expired
	}
	require.EqualValues(t, 1, priorLive)
	require.EqualValues(t, 1, priorExpired)
}
