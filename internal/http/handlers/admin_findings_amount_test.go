package handlers

import (
	"encoding/json"
	"testing"

	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/stretchr/testify/require"
)

// TestParamAmountMicrosRejectsFloats pins the or#863 fix at the exact site the
// audit found: the finding-approve refund amount is MICROS, it feeds a real
// provider refund plus an intents-ledger write, and it used to be read as
// `int64(raw.(float64))`. 60_000_000 micros survived that; 9_007_199_254_740_993
// did not, and neither did anything with a fractional part — both silently.
func TestParamAmountMicrosRejectsFloats(t *testing.T) {
	t.Parallel()

	// The exact truncation the old code performed, spelled out: a float64 can
	// no longer represent this micros value, so int64(f) is off by one.
	const beyondFloat64 int64 = 9_007_199_254_740_993 // 2^53 + 1
	require.NotEqual(t, beyondFloat64, int64(float64(beyondFloat64)),
		"precondition: this value must not survive a float64 round trip")

	got, err := paramAmountMicros(json.Number("9007199254740993"))
	require.NoError(t, err)
	require.Equal(t, beyondFloat64, got, "an exact json.Number must survive verbatim")

	// A float64 is refused outright rather than truncated.
	_, err = paramAmountMicros(float64(60_000_000))
	require.Error(t, err)
	require.Contains(t, err.Error(), "floating-point")

	// A fractional literal is not a whole number of micros — refused, not floored.
	_, err = paramAmountMicros(json.Number("60000000.5"))
	require.Error(t, err)

	// A decimal string is accepted for clients keeping money out of JSON numbers.
	got, err = paramAmountMicros(" 19990000 ")
	require.NoError(t, err)
	require.Equal(t, int64(19_990_000), got)

	for _, bad := range []any{json.Number("0"), json.Number("-1"), "abc", true, nil, map[string]any{}} {
		_, err := paramAmountMicros(bad)
		require.Error(t, err, "%v must not parse as an amount", bad)
	}
}

// TestOverrideParamsDecodeExactly pins the other half: operator override_params
// arrive as raw JSON and are decoded with UseNumber, so an integer amount
// literal reaches the executor as an exact json.Number instead of a float64.
// Plain json.Unmarshal (what the handler used to bind) loses the value.
func TestOverrideParamsDecodeExactly(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"amount": 9007199254740993}`)

	var lossy map[string]any
	require.NoError(t, json.Unmarshal(raw, &lossy))
	require.IsType(t, float64(0), lossy["amount"], "precondition: plain binding yields float64")
	require.NotEqual(t, int64(9_007_199_254_740_993), int64(lossy["amount"].(float64)))

	exact, err := recommend.DecodeParams(raw)
	require.NoError(t, err)
	require.IsType(t, json.Number(""), exact["amount"])

	micros, err := paramAmountMicros(exact["amount"])
	require.NoError(t, err)
	require.Equal(t, int64(9_007_199_254_740_993), micros)

	// A recommendation carried in finding evidence takes the same route.
	rec, ok := recommend.FromEvidence(map[string]any{
		"recommendation": map[string]any{
			"action": recommend.ActionCancelAndRefund,
			"params": map[string]any{"amount": json.RawMessage(`9007199254740993`)},
		},
	})
	require.True(t, ok)
	micros, err = paramAmountMicros(rec.Params["amount"])
	require.NoError(t, err)
	require.Equal(t, int64(9_007_199_254_740_993), micros)

	empty, err := recommend.DecodeParams(nil)
	require.NoError(t, err)
	require.Nil(t, empty)
}
