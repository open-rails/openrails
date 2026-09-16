package moneyutil

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatMicrosDecimal(t *testing.T) {
	require.Equal(t, "0.000000", FormatMicrosDecimal(0))
	require.Equal(t, "12.340000", FormatMicrosDecimal(12_340_000))
	require.Equal(t, "-12.340001", FormatMicrosDecimal(-12_340_001))
}

func TestCentMicrosConversion(t *testing.T) {
	// GAP-12: these are the typed unit boundary. The expectations are typed
	// too, so a signature that silently reverted to int64 fails here.
	require.Equal(t, Micros(12_340_000), CentsToMicros(1234))

	ceil, err := NativeToRailMinor("USD", 12_340_000)
	require.NoError(t, err)
	require.Equal(t, Cents(1234), ceil)
	ceil, err = NativeToRailMinor("USD", 12_340_001)
	require.NoError(t, err)
	require.Equal(t, Cents(1235), ceil)

	got, err := NativeToRailMinorExact("USD", 12_340_000)
	require.NoError(t, err)
	require.Equal(t, Cents(1234), got)
	_, err = NativeToRailMinorExact("USD", 12_340_001)
	require.Error(t, err)
}

func TestFormatCentsDecimal(t *testing.T) {
	require.Equal(t, "0.00", FormatCentsDecimal(0))
	require.Equal(t, "12.34", FormatCentsDecimal(1234))
	require.Equal(t, "-12.34", FormatCentsDecimal(-1234))
}

func TestFormatUSD(t *testing.T) {
	require.Equal(t, "$12.340000", FormatUSD(12_340_000))
	require.Equal(t, "-$12.340000", FormatUSD(-12_340_000))
}

func TestFormatDecimalInt64Extremes(t *testing.T) {
	require.Equal(t, "-9223372036854.775808", FormatMicrosDecimal(math.MinInt64))
	require.Equal(t, "9223372036854.775807", FormatMicrosDecimal(math.MaxInt64))
	require.Equal(t, "-92233720368547758.08", FormatCentsDecimal(math.MinInt64))
	require.Equal(t, "-0.01", FormatCentsDecimal(-1))
}
