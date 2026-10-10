//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// One PSP whose credentials cannot be checked never takes checkout down: the
// document still lists every other PSP with its public values, and that one as
// temporarily unavailable, without them.
func TestPublicConfigDegradesOnePSP(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t)
	// An edit outside OpenRails leaves the CCBill DataLink pair half set.
	w.editDoc("psps/ccbill", func(doc map[string]any) {
		secrets, _ := doc["secrets"].(map[string]any)
		secrets["datalink_username"] = "half-a-pair"
	})

	for _, tp := range []topology{embedded} {
		cfg := publicConfig(t, w.client[tp])
		byRail := map[string]billing.PSPPaymentConfig{}
		for _, psp := range cfg.Payment.PSPs {
			byRail[psp.Rail] = psp
		}
		require.Empty(t, byRail["stripe"].Status, "%s: the other PSPs stay available", tp)
		ccbill, ok := byRail["ccbill"]
		require.True(t, ok, "%s: the failing PSP is still listed: %+v", tp, cfg.Payment.PSPs)
		require.Equal(t, billing.PSPTemporarilyUnavailable, ccbill.Status, tp)
		require.Positive(t, ccbill.RetryAfter, tp)
		require.Empty(t, ccbill.Config, "%s: no values to drive an unavailable PSP", tp)
	}
}
