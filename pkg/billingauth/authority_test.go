package billingauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	auth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/stretchr/testify/require"
)

func TestHasPermissionWildcards(t *testing.T) {
	for grant, cases := range map[string]map[string]bool{
		"billing:read":     {"billing:read": true, "billing:read:extra": false},
		"  billing:read  ": {"billing:read": true},
		"*":                {"anything:at:all": true},
		"billing:*":        {"billing:read": true, "billing:admin:refund": true, "billingx:read": false, "catalog:read": false},
		"bill*":            {"billing:read": false},
	} {
		for perm, want := range cases {
			require.Equal(t, want, HasPermission([]string{grant}, perm), "%q covers %q", grant, perm)
		}
	}
	require.False(t, HasPermission(nil, "billing:read"))
}

func TestDelegatedGateAndExplicitMapping(t *testing.T) {
	for _, p := range []*DelegatedPrincipal{nil, {SubjectID: "s"}, {MerchantID: " ", SubjectID: "s"}, {MerchantID: "m", SubjectID: " "}, {MerchantID: "m", SubjectID: "s", CredentialClass: "admin"}} {
		require.ErrorIs(t, p.Validate(), ErrDelegatedPrincipalInvalid)
	}
	require.NoError(t, (&DelegatedPrincipal{MerchantID: "m", SubjectID: "s", CredentialClass: CredentialClassAutomation}).Validate())

	base := DelegatedPrincipal{MerchantID: "00000000-0000-0000-0000-000000000001", MerchantSlug: "host-one", SubjectID: "user-1", Permissions: []string{"billing:*"}, Email: "u@example.com", Username: "u"}
	gate := func(mutate func(*DelegatedPrincipal), err error) DelegatedGate {
		return NewDelegatedGate(DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*DelegatedPrincipal, error) {
			p := base
			if mutate != nil {
				mutate(&p)
			}
			return &p, err
		}))
	}
	req := httptest.NewRequest(http.MethodGet, "/billing/v1/me", nil)
	for _, tc := range []struct {
		gate DelegatedGate
		want GateError
	}{
		{NewDelegatedGate(nil), GateError{500, "authorization unavailable"}},
		{gate(nil, ErrUnauthenticated), GateError{401, "authentication required"}},
		{gate(func(p *DelegatedPrincipal) { p.Permissions = []string{"catalog:read"} }, nil), GateError{403, "permission_required"}},
		{gate(func(p *DelegatedPrincipal) { p.MerchantID = "not-a-merchant-id" }, nil), GateError{401, "delegated_principal_invalid"}},
	} {
		_, err := tc.gate.Authorize(context.Background(), req, "billing:read")
		require.Equal(t, tc.want, err)
	}
	got, err := gate(nil, nil).Authorize(context.Background(), req, "billing:read")
	require.NoError(t, err)
	require.Equal(t, base.MerchantID, got.MerchantID.String())
	require.Equal(t, []string{"billing:*"}, got.Permissions)
	require.Equal(t, UserContext{UserID: "user-1", Email: "u@example.com", Username: "u", Merchant: "host-one"}, got.UserContext)
}

type fakeVerifier struct {
	calls     int
	principal auth.Principal
	err       error
}

func (v *fakeVerifier) AuthenticateRequest(context.Context, *http.Request) (auth.Principal, error) {
	v.calls++
	return v.principal, v.err
}

type identityOnly struct{ identity auth.Identity }

func (p *identityOnly) Identity() auth.Identity { return p.identity }

type checkingPrincipal struct {
	identityOnly
	scopes  []string
	allowed bool
	err     error
}

func (p *checkingPrincipal) Can(_ context.Context, scope auth.Scope, _ string) (bool, error) {
	p.scopes = append(p.scopes, scope.ID)
	return p.allowed, p.err
}

const (
	testIssuer   = "https://issuer.test"
	testCustomer = "11111111-1111-4111-8111-111111111111"
)

func userPrincipal(allowed bool) *checkingPrincipal {
	return &checkingPrincipal{identityOnly: identityOnly{auth.Identity{Kind: auth.KindUser, Subject: testCustomer, Issuer: testIssuer}}, allowed: allowed}
}

func fixedAuthority(id string) AuthorityResolver {
	return func(context.Context, Requirement) (Authority, error) {
		return Authority{Scope: auth.Scope{Authority: testIssuer, ID: id}, Permission: "merchant:payments:refund"}, nil
	}
}

func authenticate(t *testing.T, opts IntegrationOptions) (*Integration, *http.Request, Identity, error) {
	t.Helper()
	integration, err := NewIntegration(opts)
	require.NoError(t, err)
	r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/merchant/payments", nil))
	identity, err := integration.Authentication.AuthenticateRequest(r.Context(), r)
	return integration, r, identity, err
}

func requireGateStatus(t *testing.T, err error, status int) {
	t.Helper()
	var ge GateError
	require.ErrorAs(t, err, &ge)
	require.Equal(t, status, ge.Status, ge.Message)
}

func TestIntegrationVerifiesOncePerRequestAndChecksAuthorityLive(t *testing.T) {
	principal := userPrincipal(true)
	verifier := &fakeVerifier{principal: principal}
	integration, r, identity, err := authenticate(t, IntegrationOptions{Verifier: verifier, Customer: SubjectCustomerID, Authority: fixedAuthority("g")})
	require.NoError(t, err)
	require.Equal(t, Identity{Kind: NativeUser, SubjectID: testCustomer, CustomerID: testCustomer, Issuer: testIssuer, CredentialClass: CredentialClassUserSession}, identity)

	need := Requirement{Permission: "merchant:payments:refund"}
	for _, mutate := range []func(*Identity){
		func(i *Identity) { i.CustomerID = "22222222-2222-4222-8222-222222222222" },
		func(i *Identity) { i.Issuer = "another" },
		func(i *Identity) { i.SubjectID = "another" },
		func(i *Identity) { i.Kind = Machine },
		func(i *Identity) { i.CredentialClass = CredentialClassAutomation },
		func(i *Identity) { i.Invoker = "other" },
		func(i *Identity) { i.Permissions = []string{"merchant:*"} },
	} {
		forged := identity
		mutate(&forged)
		requireGateStatus(t, integration.Authorization.Authorize(r.Context(), r, forged, need), 401)
	}
	require.NoError(t, integration.Authorization.Authorize(r.Context(), r, identity, need))
	principal.allowed = false
	requireGateStatus(t, integration.Authorization.Authorize(r.Context(), r, identity, need), 403)
	require.Equal(t, 1, verifier.calls, "proof verified once per request")
	require.Len(t, principal.scopes, 2, "permission decisions are never cached")
}

func TestIntegrationIdentityMapping(t *testing.T) {
	_, err := NewIntegration(IntegrationOptions{Verifier: (*fakeVerifier)(nil)})
	require.ErrorContains(t, err, "verifier is required")
	noAuthz, _, _, _ := authenticate(t, IntegrationOptions{Verifier: &fakeVerifier{principal: userPrincipal(true)}})
	require.Nil(t, noAuthz.Authorization, "no authority resolver publishes no privileged routes")

	claimSession := func(context.Context, auth.Principal) (CustomerIdentity, error) {
		return CustomerIdentity{ID: testCustomer, CredentialClass: CredentialClassUserSession}, nil
	}
	id := func(kind auth.Kind, subject, issuer string) auth.Identity {
		return auth.Identity{Kind: kind, Subject: subject, Issuer: issuer}
	}
	for _, tc := range []struct {
		identity auth.Identity
		customer CustomerResolver
		wantKind PrincipalKind // empty = ErrUnauthenticated
	}{
		{id(auth.KindAPIKey, "key", testIssuer), nil, Machine},
		{id(auth.KindRemoteApplication, "app", testIssuer), nil, Machine},
		{id(auth.KindDeviceKey, "dev", testIssuer), nil, Machine},
		{id(auth.KindDelegated, "sub", testIssuer), nil, DelegatedUser},
		{id(auth.KindAPIKey, testCustomer, testIssuer), SubjectCustomerID, Machine},
		{id("robot", "x", testIssuer), nil, ""},
		{id(auth.KindUser, testCustomer, ""), nil, ""},
		{id(auth.KindUser, "", testIssuer), nil, ""},
		{id(auth.KindAPIKey, "key", testIssuer), claimSession, ""},
		{id(auth.KindDelegated, "sub", testIssuer), claimSession, ""},
		{id(auth.KindUser, "11111111111141118111111111111111", testIssuer), SubjectCustomerID, ""},
		{id(auth.KindUser, "00000000-0000-0000-0000-000000000000", testIssuer), SubjectCustomerID, ""},
	} {
		_, _, got, err := authenticate(t, IntegrationOptions{Verifier: &fakeVerifier{principal: &identityOnly{tc.identity}}, Customer: tc.customer})
		if tc.wantKind == "" {
			require.ErrorIs(t, err, ErrUnauthenticated, "%+v", tc.identity)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, tc.wantKind, got.Kind)
		require.Equal(t, CredentialClassAutomation, got.CredentialClass, "non-user credentials default to automation")
		require.Empty(t, got.CustomerID, "SubjectCustomerID maps only native users")
	}
}

func TestIntegrationClassifiesVerifierFailures(t *testing.T) {
	var typedNil *identityOnly
	for _, tc := range []struct {
		principal auth.Principal
		err       error
		want      error
	}{
		{nil, fmt.Errorf("provider: %w", auth.ErrForbidden), GateError{403, "permission_required"}},
		{nil, auth.ErrUnavailable, GateError{503, "authentication unavailable"}},
		{nil, auth.ErrSenderProofRequired, GateError{401, "sender_proof_required"}},
		{nil, auth.ErrExpired, GateError{401, "credential_expired"}},
		{nil, auth.ErrRevoked, GateError{401, "credential_revoked"}},
		{nil, errors.New("db password=hunter2"), ErrUnauthenticated},
		{nil, nil, ErrUnauthenticated},
		{typedNil, nil, ErrUnauthenticated},
	} {
		verifier := &fakeVerifier{principal: tc.principal, err: tc.err}
		integration, r, _, err := authenticate(t, IntegrationOptions{Verifier: verifier})
		require.Equal(t, tc.want, err)
		_, err = integration.Authentication.AuthenticateRequest(r.Context(), r)
		require.Equal(t, tc.want, err)
		require.Equal(t, 1, verifier.calls, "failures are cached for the request too")
	}
}

func TestIntegrationAuthorityResolution(t *testing.T) {
	failing := AuthorityResolver(func(context.Context, Requirement) (Authority, error) { return Authority{}, errors.New("down") })
	empty := AuthorityResolver(func(context.Context, Requirement) (Authority, error) { return Authority{}, nil })
	var platformQuery Requirement
	platform := AuthorityResolver(func(_ context.Context, q Requirement) (Authority, error) {
		platformQuery = q
		return Authority{Scope: auth.Scope{Authority: testIssuer, ID: "root"}, Permission: "root:merchants:read"}, nil
	})
	for _, tc := range []struct {
		name                string
		principal           auth.Principal
		authority, platform AuthorityResolver
		boundGroup          string
		want                int
		wantScopes          []string
	}{
		{"principal without checker", &identityOnly{userPrincipal(true).identity}, fixedAuthority("g"), nil, "", 403, nil},
		{"resolver failure", userPrincipal(true), failing, nil, "", 503, nil},
		{"unmapped requirement", userPrincipal(true), empty, nil, "", 403, nil},
		{"checker failure", &checkingPrincipal{identityOnly: userPrincipal(true).identityOnly, err: errors.New("down")}, fixedAuthority("g"), nil, "", 503, []string{"g"}},
		{"bound group mismatch", userPrincipal(true), fixedAuthority("other"), nil, "bound", 403, nil},
		{"bound group match", userPrincipal(true), fixedAuthority("bound"), nil, "bound", 0, []string{"bound"}},
		{"mismatch falls through to platform", userPrincipal(false), fixedAuthority("g"), platform, "bound", 403, []string{"root"}},
		{"platform alone", userPrincipal(true), nil, platform, "", 0, []string{"root"}},
	} {
		platformQuery = Requirement{}
		integration, r, identity, err := authenticate(t, IntegrationOptions{Verifier: &fakeVerifier{principal: tc.principal}, Authority: tc.authority, PlatformAuthority: tc.platform})
		require.NoError(t, err)
		need := Requirement{Permission: "merchant:payments:refund", Scope: MerchantScope, Target: Target{AuthorityGroupID: tc.boundGroup}}
		err = integration.Authorization.Authorize(r.Context(), r, identity, need)
		if tc.want == 0 {
			require.NoError(t, err, tc.name)
		} else {
			requireGateStatus(t, err, tc.want)
		}
		if cp, ok := tc.principal.(*checkingPrincipal); ok {
			require.Equal(t, tc.wantScopes, cp.scopes, tc.name)
		}
		if tc.platform != nil {
			require.Equal(t, PlatformScope, platformQuery.Scope, tc.name)
		}
	}
}
