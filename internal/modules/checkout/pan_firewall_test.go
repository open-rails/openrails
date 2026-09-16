package checkout

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPANFirewallPreservesCanonicalHandles(t *testing.T) {
	const handle = "abcdefab-cdef-4abc-8111-111111111112"
	require.True(t, looksLikePAN(handle), "this UUID has a Luhn-valid suffix that reproduced the false positive")
	require.NoError(t, RejectPANShapedFields(&CheckoutRequest{BTTokenIntentID: handle}))
	require.NoError(t, RejectPANShapedFields(&CheckoutRequest{PaymentMethodID: "pm_" + handle}))
	require.NoError(t, RejectPANShapedFields(&CheckoutRequest{PaymentMethodID: handle}))
	require.Error(t, RejectPANShapedFields(&CheckoutRequest{BTTokenIntentID: "4111-1111-1111-1111"}))
	require.Error(t, RejectPANShapedFields(&CheckoutRequest{PaymentMethodID: "pm_4111111111111111"}))
	require.Error(t, RejectPANShapedFields(&CheckoutRequest{BTTokenIntentID: handle, Metadata: map[string]string{"note": "4111111111111111"}}))
	require.Error(t, RejectPANShapedFields(&CheckoutRequest{BTTokenIntentID: handle, NameOnCard: "Cardholder 4111 1111 1111 1111"}))
}
