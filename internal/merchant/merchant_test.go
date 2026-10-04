package merchant

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// There is no default merchant: absent or zero ids are unresolved, and the
// general and Host-pinned keys never stand in for each other.
func TestContextNeverFallsBack(t *testing.T) {
	id := billing.MerchantID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	var nilCtx context.Context
	for _, ctx := range []context.Context{nilCtx, context.Background(), WithID(context.Background(), billing.MerchantID{}), WithHostMerchant(context.Background(), id)} {
		_, ok := FromContext(ctx)
		require.False(t, ok)
		_, err := Require(ctx)
		require.ErrorIs(t, err, ErrNoMerchant)
	}
	got, err := Require(WithID(context.Background(), id))
	require.NoError(t, err)
	require.Equal(t, id, got)

	for _, ctx := range []context.Context{nilCtx, context.Background(), WithHostMerchant(context.Background(), billing.MerchantID{}), WithID(context.Background(), id)} {
		_, ok := HostMerchant(ctx)
		require.False(t, ok)
	}
	host, ok := HostMerchant(WithHostMerchant(context.Background(), id))
	require.True(t, ok)
	require.Equal(t, id, host)
}
