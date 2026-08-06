package service

import (
	"context"
	"fmt"
	"strings"
)

// CheckoutCustomerIdentityResolver lets an embedded host resolve verified
// customer attributes from its own server-side identity system. Browser form
// values are billing input, not authoritative identity. The returned identity
// must retain customerRef as its ID; OpenRails rejects mismatches before
// creating a checkout session.
type CheckoutCustomerIdentityResolver interface {
	ResolveCheckoutCustomerIdentity(ctx context.Context, customerRef string) (CheckoutCustomerIdentity, error)
}

// CheckoutCustomerIdentityResolverFunc adapts a function to
// CheckoutCustomerIdentityResolver.
type CheckoutCustomerIdentityResolverFunc func(ctx context.Context, customerRef string) (CheckoutCustomerIdentity, error)

// ResolveCheckoutCustomerIdentity implements CheckoutCustomerIdentityResolver.
func (f CheckoutCustomerIdentityResolverFunc) ResolveCheckoutCustomerIdentity(
	ctx context.Context,
	customerRef string,
) (CheckoutCustomerIdentity, error) {
	if f == nil {
		return CheckoutCustomerIdentity{}, fmt.Errorf("checkout customer identity resolver required")
	}
	return f(ctx, customerRef)
}

func resolveCheckoutCustomerIdentity(
	ctx context.Context,
	customerRef string,
	resolver CheckoutCustomerIdentityResolver,
) (CheckoutCustomerIdentity, error) {
	customerRef = strings.TrimSpace(customerRef)
	if customerRef == "" {
		return CheckoutCustomerIdentity{}, fmt.Errorf("customer_ref required")
	}
	if resolver == nil {
		return CheckoutCustomerIdentity{}, fmt.Errorf("checkout customer identity resolver required")
	}
	identity, err := resolver.ResolveCheckoutCustomerIdentity(ctx, customerRef)
	if err != nil {
		return CheckoutCustomerIdentity{}, fmt.Errorf("resolve checkout customer identity: %w", err)
	}
	if strings.TrimSpace(identity.ID) != customerRef {
		return CheckoutCustomerIdentity{}, fmt.Errorf("resolved checkout customer id does not match customer_ref")
	}
	identity.ID = customerRef
	return identity, nil
}
