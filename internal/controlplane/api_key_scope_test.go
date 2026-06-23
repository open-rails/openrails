package controlplane

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

// TestResolvedServiceCredentialAllowsCustomerScopes asserts the #569 (hard cut)
// behavior: a resolved merchant credential is MERCHANT-WIDE. There is no
// "merchant key scoped to one customer" concept anymore (resource scopes were
// removed), so a credential with a non-zero MerchantID may act for ANY non-nil
// payable subject within its merchant. It allows nothing when its MerchantID is
// zero or the subject is nil.
func TestResolvedServiceCredentialAllowsCustomerScopes(t *testing.T) {
	subject := uuid.New()
	other := uuid.New()

	merchantWide := &ResolvedServiceCredential{MerchantID: dbtest.TestMerchantID}
	// Merchant-wide: every non-nil subject inside the merchant is allowed.
	require.True(t, merchantWide.AllowsCustomer(subject))
	require.True(t, merchantWide.AllowsCustomer(other))

	// A nil subject is never allowed.
	require.False(t, merchantWide.AllowsCustomer(uuid.Nil))

	// A credential with no resolved merchant allows nothing.
	noMerchant := &ResolvedServiceCredential{}
	require.False(t, noMerchant.AllowsCustomer(subject))

	// A nil credential allows nothing.
	var nilCred *ResolvedServiceCredential
	require.False(t, nilCred.AllowsCustomer(subject))
}
