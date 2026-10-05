//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// SEC-33: provider account ids are global and CCBill events route by account
// id alone. A merchant cannot claim an account through the merchant API
// without credentials that prove control of it, so a squatter cannot register
// another merchant's CCBill account first and receive its events. The
// deployment operator's declaration remains the approved path.
func TestSecurityProviderAccountClaimsNeedProof(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	r := w.rival()
	for _, account := range []string{"999999-0001", "999999-0000"} {
		_, err := r.client.CreatePSP(t.Context(), billing.CreatePSPParams{OperationID: uuid.New(), Key: "squat", Rail: billing.RailCCBill, AccountID: account})
		require.Error(t, err, "an unproven claim of %s is refused", account)
	}
	list, err := r.client.ListPSPs(t.Context(), billing.PSPListParams{Rail: billing.RailCCBill})
	require.NoError(t, err)
	for _, psp := range list.Items {
		require.NotContains(t, []string{"999999-0001", "999999-0000"}, psp.AccountID)
	}
	// The operator-declared account keeps working for its merchant.
	m := importCCBill(t, w)
	require.NotNil(t, m)
}
