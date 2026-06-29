package moneyutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDecimalToMicros(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "whole dollars", raw: "10", want: 10_000_000},
		{name: "six decimals", raw: "10.000001", want: 10_000_001},
		{name: "seven decimals rounds up", raw: "10.0000005", want: 10_000_001},
		{name: "seven decimals rounds down", raw: "10.0000004", want: 10_000_000},
		{name: "negative rounds away from zero", raw: "-1.0000005", want: -1_000_001},
		{name: "invalid", raw: "abc", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDecimalToMicros(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFormatMicrosDecimal(t *testing.T) {
	require.Equal(t, "0.000000", FormatMicrosDecimal(0))
	require.Equal(t, "12.340000", FormatMicrosDecimal(12_340_000))
	require.Equal(t, "-12.340001", FormatMicrosDecimal(-12_340_001))
}

func TestCentMicrosConversion(t *testing.T) {
	require.Equal(t, int64(12_340_000), CentsToMicros(1234))
	require.Equal(t, int64(1234), MicrosToCentsCeil(12_340_000))
	require.Equal(t, int64(1235), MicrosToCentsCeil(12_340_001))

	got, err := MicrosToCentsExact(12_340_000)
	require.NoError(t, err)
	require.Equal(t, int64(1234), got)
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
