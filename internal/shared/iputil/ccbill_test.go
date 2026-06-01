package iputil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsValidCCBillIPRejectsForwardedHeaderLists(t *testing.T) {
	require.True(t, IsValidCCBillIP("64.38.212.1"))
	require.False(t, IsValidCCBillIP("64.38.212.1, 203.0.113.9"))
}

func TestConfigureRejectsInvalidCIDR(t *testing.T) {
	err := Configure([]string{"203.0.113.0/24", "not-a-cidr"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not-a-cidr")
}

func TestConfigureDrivenAllowlist(t *testing.T) {
	// Restore the default allowlist after the test so other tests are unaffected.
	t.Cleanup(func() {
		require.NoError(t, Configure(nil))
	})

	// A config-driven allowlist replaces the built-in defaults entirely.
	require.NoError(t, Configure([]string{"203.0.113.0/24"}))

	// IPs in the configured range are accepted...
	require.True(t, IsValidCCBillIP("203.0.113.7"))
	// ...and the previous default ranges are now rejected.
	require.False(t, IsValidCCBillIP("64.38.212.1"))
	// Unknown IPs are rejected.
	require.False(t, IsValidCCBillIP("198.51.100.4"))
}

func TestConfigureEmptyFallsBackToDefaults(t *testing.T) {
	require.NoError(t, Configure(nil))
	require.True(t, IsValidCCBillIP("64.38.240.10"))
	require.False(t, IsValidCCBillIP("203.0.113.7"))
}
