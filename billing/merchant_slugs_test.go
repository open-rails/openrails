package billing

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMerchantIDWire(t *testing.T) {
	id, err := ParseMerchantID("11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	require.Equal(t, "11111111-1111-1111-1111-111111111111", id.String())
	require.False(t, id.IsZero())
	require.True(t, MerchantID{}.IsZero())
	_, err = ParseMerchantID("not-a-uuid")
	require.Error(t, err)
	raw, err := json.Marshal(map[string]MerchantID{"merchant_id": id})
	require.NoError(t, err)
	require.JSONEq(t, `{"merchant_id":"11111111-1111-1111-1111-111111111111"}`, string(raw))
	var back map[string]MerchantID
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, id, back["merchant_id"])
}

// Slugs mirror AuthKit permission-group instance slugs, checked after trim+lowercase.
func TestValidateMerchantSlug(t *testing.T) {
	for _, s := range []string{"a", "ab", "a-b-c", "x1-2y", "host-four", "  Host-Four  ", "UPPER", "a--b", strings.Repeat("a", 63)} {
		require.NoError(t, ValidateMerchantSlug(s), s)
	}
	for _, s := range []string{"", " ", "-x", "x-", "a_b", "a.b", "a b", "a/b", "héllo", "a--b-", strings.Repeat("a", 64)} {
		require.Error(t, ValidateMerchantSlug(s), s)
	}
	require.Equal(t, "host-four", NormalizeMerchantSlug("  Host-Four "))
	for _, reserved := range ReservedMerchantSlugs {
		require.NoError(t, ValidateMerchantSlug(reserved), "reserved entries must be reachable slugs")
	}
}
