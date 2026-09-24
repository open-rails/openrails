package merchant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// There is no default merchant: absent or zero ids are unresolved, and the
// general and Host-pinned keys never stand in for each other.
func TestContextNeverFallsBack(t *testing.T) {
	id := ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	var nilCtx context.Context
	for _, ctx := range []context.Context{nilCtx, context.Background(), WithID(context.Background(), ID{}), WithHostMerchant(context.Background(), id)} {
		_, ok := FromContext(ctx)
		require.False(t, ok)
		_, err := Require(ctx)
		require.ErrorIs(t, err, ErrNoMerchant)
	}
	got, err := Require(WithID(context.Background(), id))
	require.NoError(t, err)
	require.Equal(t, id, got)

	for _, ctx := range []context.Context{nilCtx, context.Background(), WithHostMerchant(context.Background(), ID{}), WithID(context.Background(), id)} {
		_, ok := HostMerchant(ctx)
		require.False(t, ok)
	}
	host, ok := HostMerchant(WithHostMerchant(context.Background(), id))
	require.True(t, ok)
	require.Equal(t, id, host)
}

func TestIDWire(t *testing.T) {
	id, err := ParseID("11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	require.Equal(t, "11111111-1111-1111-1111-111111111111", id.String())
	require.False(t, id.IsZero())
	require.True(t, ID{}.IsZero())
	_, err = ParseID("not-a-uuid")
	require.Error(t, err)
	raw, err := json.Marshal(map[string]ID{"merchant_id": id})
	require.NoError(t, err)
	require.JSONEq(t, `{"merchant_id":"11111111-1111-1111-1111-111111111111"}`, string(raw))
	var back map[string]ID
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, id, back["merchant_id"])
}

// Slugs mirror AuthKit permission-group instance slugs, checked after trim+lowercase.
func TestValidateSlug(t *testing.T) {
	for _, s := range []string{"a", "ab", "a-b-c", "x1-2y", "host-four", "  Host-Four  ", "UPPER", "a--b", strings.Repeat("a", 63)} {
		require.NoError(t, ValidateSlug(s), s)
	}
	for _, s := range []string{"", " ", "-x", "x-", "a_b", "a.b", "a b", "a/b", "héllo", "a--b-", strings.Repeat("a", 64)} {
		require.Error(t, ValidateSlug(s), s)
	}
	require.Equal(t, "host-four", NormalizeSlug("  Host-Four "))
	for _, reserved := range ReservedHostedSlugs {
		require.NoError(t, ValidateSlug(reserved), "reserved entries must be reachable slugs")
	}
}
