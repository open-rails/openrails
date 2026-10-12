package billing

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImmutablePriceID(t *testing.T) {
	for _, tc := range []struct {
		product, price string
		version        int64
		want           string
	}{
		{"premium", "buy-usd", 1, "premium:buy-usd:v1"},
		{"premium", "buy-usd", 12, "premium:buy-usd:v12"},
		{"book:é/100%", "buy:USD/+", 1, "book%3A%C3%A9%2F100%25:buy%3AUSD%2F%2B:v1"},
	} {
		id, err := NewPriceID(tc.product, tc.price, tc.version)
		require.NoError(t, err)
		require.Equal(t, tc.want, id.String())
		parsed, err := ParsePriceID(tc.want)
		require.NoError(t, err)
		require.Equal(t, id, parsed)
		encoded, err := json.Marshal(id)
		require.NoError(t, err)
		var decoded PriceID
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		require.Equal(t, id, decoded)
	}
	for _, invalid := range []string{
		"price_11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-111111111111",
		"premium:buy-usd", "premium:buy-usd:v0", "premium:buy-usd:v01", "premium:buy-usd:v-1",
		"premium:buy-usd:v+1", "premium:buy-usd:v9223372036854775808", "premium::v1",
		"premium:buy:usd:v1", "%70remium:buy-usd:v1", "book%3a100:buy-usd:v1",
		"book/100:buy-usd:v1", "premium:buy-usd:v1 ", "%FF:buy-usd:v1", "%00:buy-usd:v1",
	} {
		_, err := ParsePriceID(invalid)
		require.Error(t, err, invalid)
		var decoded PriceID
		encoded, err := json.Marshal(invalid)
		require.NoError(t, err)
		require.Error(t, json.Unmarshal(encoded, &decoded), invalid)
	}
	zero, err := ParsePriceID("")
	require.NoError(t, err)
	require.True(t, zero.IsZero())
}
