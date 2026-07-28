package moneyutil

import (
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
	require.Equal(t, Cents(1234), MicrosToCentsCeil(12_340_000))
	require.Equal(t, Cents(1235), MicrosToCentsCeil(12_340_001))

	got, err := MicrosToCentsExact(12_340_000)
	require.NoError(t, err)
	require.Equal(t, Cents(1234), got)
	_, err = MicrosToCentsExact(12_340_001)
	require.Error(t, err)
}

func TestFormatCentsDecimal(t *testing.T) {
	require.Equal(t, "0.00", FormatCentsDecimal(0))
	require.Equal(t, "12.34", FormatCentsDecimal(1234))
	require.Equal(t, "-12.34", FormatCentsDecimal(-1234))
}

func TestFormatDisplay(t *testing.T) {
	require.Equal(t, "$12.340000 USD", FormatDisplay(12_340_000, "usd"))
	require.Equal(t, "-$12.340000 USD", FormatDisplay(-12_340_000, "USD"))
	require.Equal(t, "12.340000 EUR", FormatDisplay(12_340_000, "eur"))
	require.Equal(t, "12.340000", FormatDisplay(12_340_000, ""))
}

func TestFormatUSD(t *testing.T) {
	require.Equal(t, "$12.340000", FormatUSD(12_340_000))
	require.Equal(t, "-$12.340000", FormatUSD(-12_340_000))
}
