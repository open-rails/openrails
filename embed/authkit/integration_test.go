package authkit

import (
	"context"
	"net/http"
	"testing"

	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestIntegrationMemoIdentityAndExplicitAdmission(t *testing.T) {
	v := &hostVerifier{claims: verify.Claims{UserID: "11111111-1111-4111-8111-111111111111", Issuer: "issuer", Roles: []string{"owner"}, Permissions: []string{"merchant:*"}}}
	auth, err := New(Config{Verifier: v})
	require.NoError(t, err)
	p := auth.Authentication.(*integration)
	r := requestauth.Begin(req("/v2/merchant/payments"))
	identity, err := p.AuthenticateRequest(r.Context(), r)
	require.NoError(t, err)
	require.Equal(t, v.claims.UserID, identity.CustomerID)
	require.Empty(t, identity.Permissions)
	for _, mutate := range []func(*billingauth.Identity){
		func(i *billingauth.Identity) { i.CustomerID = "22222222-2222-4222-8222-222222222222" },
		func(i *billingauth.Identity) { i.Issuer = "another" },
		func(i *billingauth.Identity) { i.CredentialClass = billingauth.CredentialClassAutomation },
		func(i *billingauth.Identity) { i.Invoker = "other" },
		func(i *billingauth.Identity) { i.Permissions = []string{"merchant:*"} },
	} {
		changed := identity
		mutate(&changed)
		err := p.Authorize(r.Context(), r, changed, billingauth.Requirement{Permission: "merchant:payments:refund"})
		var denied billingauth.GateError
		require.ErrorAs(t, err, &denied)
		require.Equal(t, 401, denied.Status)
	}
	err = p.Authorize(r.Context(), r, identity, billingauth.Requirement{Permission: "merchant:payments:refund"})
	var unavailable billingauth.GateError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, 503, unavailable.Status)
	require.Equal(t, 1, v.calls, "authentication and authorization verify the proof once")
	calls := 0
	optedIn, err := New(Config{Verifier: v, Admission: func(context.Context, *http.Request, verify.Claims) error {
		calls++
		return billingauth.ErrUnauthenticated
	}})
	require.NoError(t, err)
	_, err = optedIn.Authentication.AuthenticateRequest(r.Context(), r)
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
	require.Equal(t, 1, calls, "distinct opt-in admission is never bypassed by another integration's memo")
	require.Equal(t, 2, v.calls)
}

func TestIntegrationRejectsUnconfiguredRuntimeVerifier(t *testing.T) {
	var verifier *verify.Verifier
	_, err := New(Config{Verifier: verifier})
	require.ErrorContains(t, err, "verifier is required")
}
