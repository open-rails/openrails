package db

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestStoredPriceReceiptProjection(t *testing.T) {
	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	var value any
	decoder := json.NewDecoder(strings.NewReader(`{"body":"{\"id\":\"price_11111111-1111-4111-8111-111111111111\",\"version\":0,\"amount\":9007199254740993}","from_price_id":"price_11111111-1111-4111-8111-111111111111","metadata":{"price_id":"price_11111111-1111-4111-8111-111111111111"}}`))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&value))
	walkStoredPrices(value, func(got uuid.UUID) string { require.Equal(t, id, got); return "premium:buy-usd:v1" })
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	require.JSONEq(t, `{"body":"{\"amount\":9007199254740993,\"id\":\"premium:buy-usd:v1\",\"version\":1}","from_price_id":"premium:buy-usd:v1","metadata":{"price_id":"price_11111111-1111-4111-8111-111111111111"}}`, string(encoded))
}
