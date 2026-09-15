package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/authkit/jwtkit"
	"github.com/stretchr/testify/require"
)

func TestExplicitHostIssuerOwnsBridgeUserNamespace(t *testing.T) {
	signer, err := jwtkit.NewRSASigner(2048, "host-key")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwtkit.ServeJWKS(w, r, jwtkit.JWKS{Keys: []jwtkit.JWK{jwtkit.PublicToJWK(signer.PublicKey(), signer.KID(), signer.Algorithm())}})
	}))
	defer server.Close()
	v, err := NewIssuerVerifier([]string{server.URL}, "billing")
	require.NoError(t, err)
	claims := map[string]any{"iss": server.URL, "sub": "019aaaab-0000-7000-8000-000000000042", "aud": []string{"billing"}, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
	token, err := jwtkit.SignWithType(context.Background(), signer, claims, jwtkit.AccessTokenType, true)
	require.NoError(t, err)
	verified, err := v.VerifyRequest(delegatedReq(token))
	require.NoError(t, err)
	require.Equal(t, claims["sub"], verified.UserID)
	claims["aud"] = []string{"another-service"}
	token, err = jwtkit.SignWithType(context.Background(), signer, claims, jwtkit.AccessTokenType, true)
	require.NoError(t, err)
	_, err = v.VerifyRequest(delegatedReq(token))
	require.Error(t, err)
}
