//go:build greenfield && integration

package subscriptions_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
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
	zero := int64(0)
	for _, account := range []string{"945280-0001", "945280-0000"} {
		_, err := r.client.PaymentProviders.Upsert(t.Context(), "ccbill", &openrails.UpsertPaymentProviderParams{OperationID: uuid.New(), ExpectedRevision: &zero, AccountID: account})
		require.Error(t, err, "an unproven claim of %s is refused", account)
	}
	list, err := r.client.PaymentProviders.List(t.Context(), &openrails.PaymentProviderListParams{Provider: "ccbill"})
	require.NoError(t, err)
	for _, psp := range list.Data {
		require.NotContains(t, []string{"945280-0001", "945280-0000"}, psp.AccountID)
	}
	// The operator-declared account keeps working for its merchant.
	m := importCCBill(t, w)
	require.NotNil(t, m)
}
