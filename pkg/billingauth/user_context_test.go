package billingauth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserContextHasEntitlementFreshTrustsVerifiedClaimSnapshotByDefault(t *testing.T) {
	uc := UserContext{
		UserID:       "0d4cdb35-9f3f-4a16-bb83-90a8aa20a2c1",
		SessionID:    "sid_123",
		Entitlements: []string{"Premium"},
	}

	ok, err := uc.HasEntitlementFresh(context.Background(), nil, "premium")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "sid_123", uc.SessionID)
}

func TestUserContextHasEntitlementFreshUsesOptionalEmbeddedResolver(t *testing.T) {
	uc := UserContext{
		UserID:       "0d4cdb35-9f3f-4a16-bb83-90a8aa20a2c1",
		Entitlements: []string{"premium"},
	}
	resolver := EntitlementFreshnessResolverFunc(func(_ context.Context, got UserContext, entitlement string) (bool, error) {
		require.Equal(t, uc.UserID, got.UserID)
		require.Equal(t, "premium", entitlement)
		return false, nil
	})

	ok, err := uc.HasEntitlementFresh(context.Background(), resolver, "premium")
	require.NoError(t, err)
	require.False(t, ok, "freshness resolver can reflect revocation before token expiry")
}
