package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/authkit/verify"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
)

const (
	engineMerchantID = "6a68e70a-4dd9-4b39-a3ba-4657303c6f70"
	userID           = "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d"
)

type countingVerifier struct {
	claims verify.Claims
	err    error
	calls  int
}

func (v *countingVerifier) VerifyRequest(*http.Request) (verify.Claims, error) {
	v.calls++
	return v.claims, v.err
}

func bearer(path, authorization string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}

// or#913: merchant is pinned at construction, never read from the token, and
// token roles confer nothing. Credential class comes only from verified claims.
func TestDelegatedPrincipalIsPinnedAndClaimDerived(t *testing.T) {
	claims := verify.Claims{UserID: userID, Roles: []string{"owner"}, Issuer: "https://auth.host.example", Email: "user@host.example", EmailVerified: true, Username: "user"}
	a := NewDelegatedAuthenticator(DelegatedConfig{Verifier: &countingVerifier{claims: claims}, MerchantID: engineMerchantID})
	r := bearer("/v1/me/invoices/id/pay-now", "Bearer x")
	r.Header.Set("Credential-Class", "automation")
	p, err := a.AuthenticateDelegated(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, &billingauth.DelegatedPrincipal{
		CredentialClass: billingauth.CredentialClassUserSession, MerchantID: engineMerchantID, SubjectID: userID,
		Issuer: "https://auth.host.example", Email: "user@host.example", EmailVerified: true, Username: "user",
	}, p)

	for _, c := range []verify.Claims{{UserID: userID, DeviceKeyID: "dk"}, {UserID: userID, TokenType: verify.APIKeyPrincipalType}} {
		a := NewDelegatedAuthenticator(DelegatedConfig{Verifier: &countingVerifier{claims: c}, MerchantID: engineMerchantID, MerchantSlug: "acme", Issuer: " openrails:self "})
		r := bearer("/", "Bearer x")
		r.Header.Set("Credential-Class", "user_session")
		p, err := a.AuthenticateDelegated(t.Context(), r)
		require.NoError(t, err)
		require.Equal(t, billingauth.CredentialClassAutomation, p.CredentialClass, "a header cannot assert user interaction")
		require.Equal(t, "acme", p.MerchantSlug)
		require.Equal(t, "openrails:self", p.Issuer, "configured issuer overrides iss")
	}
}

func TestDelegatedAuthenticatorFailsClosed(t *testing.T) {
	boom := errors.New("boom")
	failing := func(context.Context, *http.Request, verify.Claims) ([]string, error) {
		return nil, errors.New("authority unreachable")
	}
	for _, tc := range []struct {
		a    *DelegatedAuthenticator
		want error
	}{
		{nil, billingauth.ErrUnauthenticated},
		{NewDelegatedAuthenticator(DelegatedConfig{MerchantID: engineMerchantID}), billingauth.ErrUnauthenticated},
		{NewDelegatedAuthenticator(DelegatedConfig{Verifier: &countingVerifier{err: boom}}), boom},
		{NewDelegatedAuthenticator(DelegatedConfig{Verifier: &countingVerifier{claims: verify.Claims{UserID: "  "}}}), billingauth.ErrUnauthenticated},
		{NewDelegatedAuthenticator(DelegatedConfig{Verifier: &countingVerifier{claims: verify.Claims{UserID: userID}}, Permissions: failing}), billingauth.ErrUnauthenticated},
	} {
		p, err := tc.a.AuthenticateDelegated(t.Context(), bearer("/", "Bearer x"))
		require.ErrorIs(t, err, tc.want)
		require.Nil(t, p)
	}
}

// or#918: admission vetoes a verified token before the (DB-backed) resolver runs;
// the resolver sees the live request; the host's reason is never returned.
func TestAdmissionPrecedesRequestScopedPermissions(t *testing.T) {
	lookups, live := 0, true
	a := NewDelegatedAuthenticator(DelegatedConfig{
		Verifier:   &countingVerifier{claims: verify.Claims{UserID: userID}},
		MerchantID: engineMerchantID,
		Admit: func(context.Context, *http.Request, verify.Claims) error {
			if !live {
				return errors.New("user is banned")
			}
			return nil
		},
		Permissions: func(_ context.Context, r *http.Request, _ verify.Claims) ([]string, error) {
			lookups++
			if r.URL.Path == "/billing/v1/merchant/settings" {
				return []string{permissions.CustomerAll, permissions.MerchantAll}, nil
			}
			return []string{permissions.CustomerAll}, nil
		},
	})
	self, err := a.AuthenticateDelegated(t.Context(), bearer("/billing/v1/me/balance", "Bearer x"))
	require.NoError(t, err)
	require.Equal(t, []string{permissions.CustomerAll}, self.Permissions)
	admin, err := a.AuthenticateDelegated(t.Context(), bearer("/billing/v1/merchant/settings", "Bearer x"))
	require.NoError(t, err)
	require.Equal(t, []string{permissions.CustomerAll, permissions.MerchantAll}, admin.Permissions)

	live = false
	_, err = a.AuthenticateDelegated(t.Context(), bearer("/billing/v1/merchant/settings", "Bearer x"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
	require.NotContains(t, err.Error(), "banned")
	require.Equal(t, 2, lookups, "no grant lookup for an inadmissible subject")
}

func TestUserAuthenticator(t *testing.T) {
	claims := verify.Claims{UserID: userID, Email: "e@x", Username: "u", DiscordUsername: "d", SessionID: "sid", Roles: []string{"admin"}, Entitlements: []string{"premium"}}
	uc, err := NewAuthenticator(AuthenticatorConfig{Verifier: &countingVerifier{claims: claims}}).Authenticate(t.Context(), bearer("/", "Bearer x"))
	require.NoError(t, err)
	require.Equal(t, billingauth.UserContext{UserID: userID, Email: "e@x", Username: "u", DiscordUsername: "d", SessionID: "sid", Roles: []string{"admin"}, Entitlements: []string{"premium"}}, uc)
	uc, err = NewAuthenticator(AuthenticatorConfig{Verifier: &countingVerifier{claims: claims}, OmitTokenRoles: true}).Authenticate(t.Context(), bearer("/", "Bearer x"))
	require.NoError(t, err)
	require.Nil(t, uc.Roles)

	vetoed := NewAuthenticator(AuthenticatorConfig{Verifier: &countingVerifier{claims: claims}, Admit: func(context.Context, *http.Request, verify.Claims) error { return errors.New("not live") }})
	_, err = vetoed.Authenticate(t.Context(), bearer("/", "Bearer x"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)

	// DPoP proofs are single-use and belong to the delegated verifier.
	v := &countingVerifier{claims: claims}
	_, err = NewAuthenticator(AuthenticatorConfig{Verifier: v}).Authenticate(t.Context(), bearer("/", " dpop abc"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
	require.Zero(t, v.calls)

	// One verification per authenticator per request.
	user, delegated := NewAuthenticator(AuthenticatorConfig{Verifier: v}), NewDelegatedAuthenticator(DelegatedConfig{Verifier: v, MerchantID: engineMerchantID})
	r := requestauth.Begin(bearer("/", "Bearer x"))
	for range 3 {
		_, err = user.Authenticate(r.Context(), r)
		require.NoError(t, err)
		_, err = delegated.AuthenticateDelegated(r.Context(), r)
		require.NoError(t, err)
	}
	require.Equal(t, 2, v.calls)
}

func TestIssuerVerifierEnforcesIssuerAndAudience(t *testing.T) {
	for _, issuers := range [][]string{nil, {" ", ""}} {
		_, err := NewIssuerVerifier(issuers, "billing")
		require.Error(t, err)
	}
	signer, err := jwtkit.NewRSASigner(2048, "host-key")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwtkit.ServeJWKS(w, r, jwtkit.JWKS{Keys: []jwtkit.JWK{jwtkit.PublicToJWK(signer.PublicKey(), signer.KID(), signer.Algorithm())}})
	}))
	defer server.Close()
	v, err := NewIssuerVerifier([]string{" " + server.URL + "/ "}, " billing ")
	require.NoError(t, err)

	sign := func(key string, value any) string {
		claims := map[string]any{"iss": server.URL, "sub": userID, "aud": []string{"billing"}, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
		if key != "" {
			claims[key] = value
		}
		token, err := jwtkit.SignWithType(context.Background(), signer, claims, jwtkit.AccessTokenType, true)
		require.NoError(t, err)
		return "Bearer " + token
	}
	verified, err := v.VerifyRequest(bearer("/", sign("", nil)))
	require.NoError(t, err)
	require.Equal(t, userID, verified.UserID, "the explicit host issuer owns the user namespace")
	for _, token := range []string{
		sign("aud", []string{"another-service"}),
		sign("iss", "https://evil.example"),
		sign("exp", time.Now().Add(-time.Hour).Unix()),
		sign("", nil) + "x",
		"",
	} {
		_, err := v.VerifyRequest(bearer("/", token))
		require.Error(t, err)
	}
}
