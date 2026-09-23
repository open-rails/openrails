//go:build integration

package testkit

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Principal is a framework-neutral authenticated caller fixture. It implements
// billingauth.Authentication and billingauth.DelegatedAuthenticator directly,
// so a focused test can inject it into either the embedded or HTTP surface
// without importing AuthKit or a web framework.
type Principal struct {
	Identity billingauth.Identity
	merchant merchant.ID
}

// NewPrincipal returns a native user session mapped to merchantID/customerID.
// Permissions are intentionally not placed on the native identity: OpenRails
// checks user authority against the host's authorization boundary, while
// non-user credentials may carry an explicit ceiling.
func NewPrincipal(merchantID merchant.ID, customerID string) Principal {
	return Principal{
		Identity: billingauth.Identity{
			Kind:            billingauth.NativeUser,
			SubjectID:       customerID,
			CustomerID:      customerID,
			CredentialClass: billingauth.CredentialClassUserSession,
		},
		merchant: merchantID,
	}
}

// AuthenticateRequest implements billingauth.Authentication. The fixture is
// already authenticated; request headers are deliberately ignored so tests
// cannot accidentally turn an untrusted request value into identity.
func (p Principal) AuthenticateRequest(context.Context, *http.Request) (billingauth.Identity, error) {
	return p.Identity, nil
}

// AuthenticateDelegated implements billingauth.DelegatedAuthenticator for
// customer self-service routes. The mapping is explicit and uses immutable
// IDs; it never infers a merchant from a request path or slug.
func (p Principal) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return &billingauth.DelegatedPrincipal{
		CredentialClass: billingauth.CredentialClassUserSession,
		MerchantID:      p.merchant.UUID().String(),
		SubjectID:       p.Identity.SubjectID,
		Issuer:          "testkit",
	}, nil
}

// MerchantID returns the explicit merchant bound to this fixture.
func (p Principal) MerchantID() merchant.ID { return p.merchant }

// LegacyPrincipal adapts the fixture for older merchant-gate surfaces that
// still accept billingauth.Principal. New focused tests should prefer the
// neutral Authentication interface above.
func (p Principal) LegacyPrincipal() billingauth.Principal {
	return billingauth.Principal{
		MerchantID: p.merchant,
		Subject:    p.Identity.SubjectID,
		UserContext: billingauth.UserContext{
			UserID:        p.Identity.CustomerID,
			Email:         p.Identity.Email,
			EmailVerified: p.Identity.EmailVerified,
			Username:      p.Identity.Username,
			SessionID:     p.Identity.SessionID,
		},
	}
}
