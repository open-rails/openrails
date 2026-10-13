//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// SEC-33: provider account ids are global and CCBill events route by account
// id alone. A merchant cannot claim an account through the admin API
// without credentials that prove control of it, so a squatter cannot register
// another merchant's CCBill account first and receive its events. The
// deployment operator's declaration remains the approved path.
func TestSecurityProviderAccountClaimsNeedProof(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t)
	r := w.rival()
	for _, subacc := range []string{"0001", "0000"} {
		account := "999999-" + subacc
		_, err := r.client.CreatePSP(t.Context(), billing.CreatePSPParams{Key: "squat", Rail: billing.RailCCBill, Environment: "sandbox", PSPAccount: billing.PSPAccount{ClientAccnum: "999999", ClientSubacc: subacc}})
		require.Error(t, err, "an unproven claim of %s is refused", account)
		require.NotEqual(t, "merchant_config_read_only", codeOf(err), "the claim itself is refused")
	}
	list, err := r.client.ListPSPs(t.Context(), billing.PSPListParams{Rail: billing.RailCCBill})
	require.NoError(t, err)
	for _, psp := range list.Items {
		require.NotContains(t, []string{"0001", "0000"}, psp.ClientSubacc)
	}
	// The operator-declared account keeps working for its merchant.
	m := importCCBill(t, w)
	require.NotNil(t, m)
}
