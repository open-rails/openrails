package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/billingauth"
)

const userID = "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d"

type countingVerifier struct {
	claims verify.Claims
	calls  int
}

func (v *countingVerifier) VerifyRequest(*http.Request) (verify.Claims, error) {
	v.calls++
	return v.claims, nil
}

func bearer(authorization string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", authorization)
	return r
}

func TestUserAuthenticator(t *testing.T) {
	claims := verify.Claims{Kind: iam.ActorUser, UserID: userID, Email: "e@x", Username: "u", SessionID: "sid", RootRole: "root:admin", Entitlements: []string{"premium"}}
	uc, err := NewAuthenticator(&countingVerifier{claims: claims}).Authenticate(t.Context(), bearer("Bearer x"))
	require.NoError(t, err)
	require.Equal(t, billingauth.UserContext{UserID: userID, Email: "e@x", Username: "u", SessionID: "sid", Entitlements: []string{"premium"}}, uc, "token role snapshots are never carried")

	// DPoP proofs are single-use and belong to the delegated verifier.
	v := &countingVerifier{claims: claims}
	_, err = NewAuthenticator(v).Authenticate(t.Context(), bearer(" dpop abc"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
	require.Zero(t, v.calls)

	// One verification per authenticator per request, shared by the actor.
	user := NewAuthenticator(v)
	r := requestauth.Begin(bearer("Bearer x"))
	for range 3 {
		_, err = user.Authenticate(r.Context(), r)
		require.NoError(t, err)
	}
	actor, err := user.Actor(r)
	require.NoError(t, err)
	require.Equal(t, 1, v.calls)
	require.Equal(t, userID, actor.ID())
	session, bound := actor.Session()
	require.True(t, bound, "the actor carries the token's session, so a revoked sign-in is refused")
	require.Equal(t, "sid", session.SessionID)

	_, err = NewAuthenticator(&countingVerifier{claims: verify.Claims{Kind: iam.ActorAPIKey, APIKeyID: "k"}}).Actor(bearer("Bearer x"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated, "only a user token has a user actor")
}
