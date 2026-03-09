package moneyutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDecimalToCents(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "whole dollars", raw: "10", want: 1000},
		{name: "two decimals", raw: "10.01", want: 1001},
		{name: "three decimals rounds up", raw: "10.015", want: 1002},
		{name: "three decimals rounds down", raw: "10.014", want: 1001},
		{name: "exact half up", raw: "1.005", want: 101},
		{name: "negative rounds away from zero", raw: "-1.005", want: -101},
		{name: "invalid", raw: "abc", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDecimalToCents(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFormatCentsDecimal(t *testing.T) {
	require.Equal(t, "0.00", FormatCentsDecimal(0))
	require.Equal(t, "12.34", FormatCentsDecimal(1234))
	require.Equal(t, "-12.34", FormatCentsDecimal(-1234))
}

func TestFormatDisplay(t *testing.T) {
	require.Equal(t, "$12.34 USD", FormatDisplay(1234, "usd"))
	require.Equal(t, "-$12.34 USD", FormatDisplay(-1234, "USD"))
	require.Equal(t, "12.34 EUR", FormatDisplay(1234, "eur"))
	require.Equal(t, "12.34", FormatDisplay(1234, ""))
}

func TestFormatUSD(t *testing.T) {
	require.Equal(t, "$12.34", FormatUSD(1234))
	require.Equal(t, "-$12.34", FormatUSD(-1234))
}
