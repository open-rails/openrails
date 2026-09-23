package billingauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	auth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/stretchr/testify/require"
)

type neutralVerifier struct {
	calls     int
	principal auth.Principal
}

func (v *neutralVerifier) AuthenticateRequest(context.Context, *http.Request) (auth.Principal, error) {
	v.calls++
	return v.principal, nil
}

type neutralPrincipal struct {
	identity auth.Identity
	calls    int
	allowed  bool
}

func (p *neutralPrincipal) Identity() auth.Identity { return p.identity }
func (p *neutralPrincipal) Can(context.Context, auth.Scope, string) (bool, error) {
	p.calls++
	return p.allowed, nil
}

func TestProviderMemoIdentityAndLiveAuthority(t *testing.T) {
	principal := &neutralPrincipal{identity: auth.Identity{Kind: auth.KindUser, Subject: "11111111-1111-4111-8111-111111111111", Issuer: "https://issuer.test"}, allowed: true}
	verifier := &neutralVerifier{principal: principal}
	integration, err := NewIntegration(IntegrationOptions{Verifier: verifier, Customer: SubjectCustomerID, Authority: func(context.Context, Requirement) (Authority, error) {
		return Authority{Scope: auth.Scope{Authority: "https://issuer.test", ID: "merchant-group"}, Permission: "merchant:payments:refund"}, nil
	}})
	require.NoError(t, err)
	r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/merchant/payments", nil))
	identity, err := integration.Authentication.AuthenticateRequest(r.Context(), r)
	require.NoError(t, err)
	require.Equal(t, principal.identity.Subject, identity.CustomerID)
	require.Empty(t, identity.Permissions)
	for _, mutate := range []func(*Identity){
		func(i *Identity) { i.CustomerID = "22222222-2222-4222-8222-222222222222" },
		func(i *Identity) { i.Issuer = "another" },
		func(i *Identity) { i.Kind = Machine },
		func(i *Identity) { i.CredentialClass = CredentialClassAutomation },
		func(i *Identity) { i.Invoker = "other" },
		func(i *Identity) { i.Permissions = []string{"merchant:*"} },
	} {
		changed := identity
		mutate(&changed)
		err := integration.Authorization.Authorize(r.Context(), r, changed, Requirement{Permission: "merchant:payments:refund"})
		var denied GateError
		require.ErrorAs(t, err, &denied)
		require.Equal(t, 401, denied.Status)
	}
	require.NoError(t, integration.Authorization.Authorize(r.Context(), r, identity, Requirement{Permission: "merchant:payments:refund"}))
	principal.allowed = false
	err = integration.Authorization.Authorize(r.Context(), r, identity, Requirement{Permission: "merchant:payments:refund"})
	var denied GateError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, 403, denied.Status)
	require.Equal(t, 1, verifier.calls, "proof verified once within a request")
	require.Equal(t, 2, principal.calls, "permission decisions remain live")
}

func TestProviderRejectsMissingVerifierAndMachineCustomerSession(t *testing.T) {
	var verifier *neutralVerifier
	_, err := NewIntegration(IntegrationOptions{Verifier: verifier})
	require.ErrorContains(t, err, "verifier is required")
	integration, err := NewIntegration(IntegrationOptions{Verifier: &neutralVerifier{principal: &neutralPrincipal{identity: auth.Identity{Kind: auth.KindAPIKey, Subject: "immutable-key-id", Issuer: "https://issuer.test"}}}, Customer: func(context.Context, auth.Principal) (CustomerIdentity, error) {
		return CustomerIdentity{ID: "11111111-1111-4111-8111-111111111111", CredentialClass: CredentialClassUserSession}, nil
	}})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err = integration.Authentication.AuthenticateRequest(r.Context(), r)
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestProviderRejectsNilPrincipalsWithoutPanic(t *testing.T) {
	var typedNil *neutralPrincipal
	for _, principal := range []auth.Principal{nil, typedNil} {
		verifier := &neutralVerifier{principal: principal}
		integration, err := NewIntegration(IntegrationOptions{Verifier: verifier})
		require.NoError(t, err)
		r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/", nil))
		for range 2 {
			_, err = integration.Authentication.AuthenticateRequest(r.Context(), r)
			require.ErrorIs(t, err, ErrUnauthenticated)
		}
		require.Equal(t, 1, verifier.calls)
	}
}
