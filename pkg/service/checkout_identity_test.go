package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckoutUserIdentity(t *testing.T) {
	t.Parallel()

	identity, err := checkoutUserIdentity(CheckoutCustomerIdentity{
		ID:            "  f2cd32df-d987-4c2f-942b-99e7232a0e05  ",
		VerifiedEmail: "  jane@example.com  ",
		Username:      "  jane  ",
	})
	require.NoError(t, err)
	require.Equal(t, "f2cd32df-d987-4c2f-942b-99e7232a0e05", identity.ID)
	require.NotNil(t, identity.Email)
	require.Equal(t, "jane@example.com", *identity.Email)
	require.Equal(t, "jane", identity.Username)
}

func TestCheckoutUserIdentityRequiresID(t *testing.T) {
	t.Parallel()

	_, err := checkoutUserIdentity(CheckoutCustomerIdentity{})
	require.EqualError(t, err, "user_id required")
}
