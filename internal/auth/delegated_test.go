package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit/verify"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type stubVerifier struct {
	claims verify.Claims
	err    error
}

func (s stubVerifier) Verify(string) (verify.Claims, error) { return s.claims, s.err }

func delegatedReq(bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/billing/v1/me/balance", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

const engineMerchantID = "6a68e70a-4dd9-4b39-a3ba-4657303c6f70"

// The or#913 contract: the principal's merchant is the ENGINE's bound
// merchant from construction — never anything read from the caller's token
// (tensorhub pinned the caller's org UUID and broke RLS on every request,
// th#1765). Subject is the sub claim; issuer is the verified iss; permissions
// come from the injected role mapping.
func TestDelegatedAuthenticator_MapsClaimsOntoEnginePinnedPrincipal(t *testing.T) {
	t.Parallel()

	v := stubVerifier{claims: verify.Claims{
		UserID:        "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d",
		Roles:         []string{"member"},
		Issuer:        "https://auth.host.example",
		Email:         "user@host.example",
		EmailVerified: true,
		Username:      "user",
	}}
	a := NewDelegatedAuthenticator(v, engineMerchantID, func(roles []string) []string {
		return permissions.ForRoles(roles...)
	})

	p, err := a.AuthenticateDelegated(t.Context(), delegatedReq("some.jwt.token"))
	require.NoError(t, err)
	require.NoError(t, p.Validate())
	require.Equal(t, engineMerchantID, p.MerchantID)
	require.Equal(t, "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d", p.SubjectID)
	require.Equal(t, "https://auth.host.example", p.Issuer)
	require.Equal(t, permissions.ForRoles("member"), p.Permissions)
	require.Equal(t, "user@host.example", p.Email)
	require.True(t, p.EmailVerified)
	require.Equal(t, "user", p.Username)
}

func TestDelegatedAuthenticator_FailsClosed(t *testing.T) {
	t.Parallel()

	valid := stubVerifier{claims: verify.Claims{UserID: "u"}}

	// No bearer at all.
	a := NewDelegatedAuthenticator(valid, engineMerchantID, nil)
	_, err := a.AuthenticateDelegated(t.Context(), delegatedReq(""))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)

	// Verifier rejects the token.
	bad := errors.New("boom")
	a = NewDelegatedAuthenticator(stubVerifier{err: bad}, engineMerchantID, nil)
	_, err = a.AuthenticateDelegated(t.Context(), delegatedReq("x.y.z"))
	require.ErrorIs(t, err, bad)

	// A verified token without a native user subject (e.g. a service
	// credential) has no self to act as.
	a = NewDelegatedAuthenticator(stubVerifier{claims: verify.Claims{UserID: "  "}}, engineMerchantID, nil)
	_, err = a.AuthenticateDelegated(t.Context(), delegatedReq("x.y.z"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)

	// Nil role mapping grants nothing (still authenticates for /v1/me).
	a = NewDelegatedAuthenticator(valid, engineMerchantID, nil)
	p, err := a.AuthenticateDelegated(t.Context(), delegatedReq("x.y.z"))
	require.NoError(t, err)
	require.Empty(t, p.Permissions)
}
