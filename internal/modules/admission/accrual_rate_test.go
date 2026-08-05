package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The scaling is integer arithmetic with the division LAST, so a sub-hour
// window does not round its own numerator away — $1 in a minute is $60/hour,
// not $0/hour, and a quota that read the second would never fire.
func TestRatePerHour(t *testing.T) {
	const dollar = int64(1_000_000)
	require.EqualValues(t, dollar, RatePerHour(dollar, time.Hour))
	require.EqualValues(t, 60*dollar, RatePerHour(dollar, time.Minute))
	require.EqualValues(t, dollar/24, RatePerHour(dollar, 24*time.Hour))
	require.EqualValues(t, 2*dollar, RatePerHour(dollar, 30*time.Minute))

	// Nothing accrued is no rate, whatever the window.
	require.Zero(t, RatePerHour(0, time.Minute))
	require.Zero(t, RatePerHour(-5, time.Hour))

	// A degenerate window falls back to the canonical hour rather than dividing
	// by zero.
	require.EqualValues(t, dollar, RatePerHour(dollar, 0))
}
