package service

import (
	"context"
	"errors"
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

func TestResolveCheckoutCustomerIdentity(t *testing.T) {
	t.Parallel()

	resolver := CheckoutCustomerIdentityResolverFunc(func(_ context.Context, customerRef string) (CheckoutCustomerIdentity, error) {
		return CheckoutCustomerIdentity{
			ID:            customerRef,
			VerifiedEmail: "jane@example.com",
			Username:      "jane",
		}, nil
	})

	identity, err := resolveCheckoutCustomerIdentity(t.Context(), "  f2cd32df-d987-4c2f-942b-99e7232a0e05  ", resolver)
	require.NoError(t, err)
	require.Equal(t, "f2cd32df-d987-4c2f-942b-99e7232a0e05", identity.ID)
	require.Equal(t, "jane@example.com", identity.VerifiedEmail)
	require.Equal(t, "jane", identity.Username)
}

func TestCreateCheckoutSessionWithCustomerResolverRejectsInvalidResolution(t *testing.T) {
	t.Parallel()

	resolverErr := errors.New("identity store unavailable")
	tests := []struct {
		name     string
		ref      string
		resolver CheckoutCustomerIdentityResolver
		wantErr  string
	}{
		{name: "missing customer ref", resolver: CheckoutCustomerIdentityResolverFunc(func(context.Context, string) (CheckoutCustomerIdentity, error) {
			return CheckoutCustomerIdentity{}, nil
		}), wantErr: "customer_ref required"},
		{name: "missing resolver", ref: "f2cd32df-d987-4c2f-942b-99e7232a0e05", wantErr: "checkout customer identity resolver required"},
		{name: "typed nil resolver", ref: "f2cd32df-d987-4c2f-942b-99e7232a0e05", resolver: CheckoutCustomerIdentityResolverFunc(nil), wantErr: "resolve checkout customer identity: checkout customer identity resolver required"},
		{name: "resolver failure", ref: "f2cd32df-d987-4c2f-942b-99e7232a0e05", resolver: CheckoutCustomerIdentityResolverFunc(func(context.Context, string) (CheckoutCustomerIdentity, error) {
			return CheckoutCustomerIdentity{}, resolverErr
		}), wantErr: "resolve checkout customer identity: identity store unavailable"},
		{name: "customer mismatch", ref: "f2cd32df-d987-4c2f-942b-99e7232a0e05", resolver: CheckoutCustomerIdentityResolverFunc(func(context.Context, string) (CheckoutCustomerIdentity, error) {
			return CheckoutCustomerIdentity{ID: "09d397cf-015f-41a3-8bad-685220b9b4fc"}, nil
		}), wantErr: "resolved checkout customer id does not match customer_ref"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := (&Service{}).CreateCheckoutSessionWithCustomerResolver(
				t.Context(),
				test.ref,
				test.resolver,
				CreateCheckoutSessionRequest{},
			)
			require.EqualError(t, err, test.wantErr)
		})
	}
}
