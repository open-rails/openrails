package billing

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The list envelope is {data, next_cursor}: data is never null, and
// next_cursor is null exactly on the last page.
func TestListPageWire(t *testing.T) {
	for _, tc := range []struct {
		page ListPage[string]
		wire string
	}{
		{ListPage[string]{}, `{"data":[],"next_cursor":null}`},
		{ListPage[string]{Items: []string{}}, `{"data":[],"next_cursor":null}`},
		{ListPage[string]{Items: []string{"a", "b"}, Next: "c2"}, `{"data":["a","b"],"next_cursor":"c2"}`},
	} {
		raw, err := json.Marshal(tc.page)
		require.NoError(t, err)
		require.JSONEq(t, tc.wire, string(raw))

		var back ListPage[string]
		require.NoError(t, json.Unmarshal(raw, &back))
		require.Equal(t, tc.page.Next, back.Next)
		require.Len(t, back.Items, len(tc.page.Items))
	}
	nested, err := json.Marshal(struct {
		Offers ListPage[int] `json:"offers"`
	}{})
	require.NoError(t, err)
	require.JSONEq(t, `{"offers":{"data":[],"next_cursor":null}}`, string(nested))
}
