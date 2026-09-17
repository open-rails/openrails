//go:build integration

package integrationharness

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/testauth"
	"github.com/open-rails/openrails/permissions"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestDelegationProxyStorageAndMerchantAdmission(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	f := newDelegationHTTPFixture(t, h)
	cp := embcp.Get(f.surface.App())
	sender, err := testauth.NewSender()
	require.NoError(t, err)
	token, err := f.engine.MintDelegatedAccessToken(ctx, authkit.DelegatedAccessParams{
		Audiences: []string{"openrails"}, DelegatedSubject: f.subject, Permissions: []string{permissions.MerchantAll}, ConfirmationJWKThumbprintSHA256: &sender.Thumbprint,
	})
	require.NoError(t, err)
	proxy := httptest.NewServer(http.StripPrefix("/edge", f.surface.Server().Handler()))
	t.Cleanup(proxy.Close)
	target := func(r *http.Request) string { return proxy.URL + "/edge" + r.URL.EscapedPath() }
	verify.WithDPoP(cp.Core().ClaimDPoPProof, target)(cp.DelegatedVerifier())
	request := func(proofTarget string) (int, http.Header) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/edge/v1/me/status", nil)
		require.NoError(t, err)
		proof, err := sender.Proof(req.Method, proofTarget, token)
		require.NoError(t, err)
		req.Header.Set("Authorization", "DPoP "+token)
		req.Header.Set("DPoP", proof)
		// Neither arbitrary client nor forwarding headers select the proof target.
		req.Host = "forged.example"
		req.Header.Set("Forwarded", "host=forged.example;proto=http")
		req.Header.Set("X-Forwarded-Host", "forged.example")
		response, err := proxy.Client().Do(req)
		require.NoError(t, err)
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode, response.Header
	}
	external := proxy.URL + "/edge/v1/me/status"
	status, _ := request(external)
	require.Equal(t, 200, status)
	status, _ = request(f.surface.BaseURL + "/v1/me/status")
	require.Equal(t, 401, status, "stripped target must not match external URL")

	// Deny Lua only for a dedicated Redis ACL user, preserving the shared fixture.
	// The actual AuthKit Redis proof-claim path must fail closed operationally.
	aclUser := "proof_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := base64.RawURLEncoding.EncodeToString([]byte(uuid.NewString()))
	require.NoError(t, h.Redis.Do(ctx, "ACL", "SETUSER", aclUser, "on", ">"+password, "~*", "+@all", "-eval", "-evalsha").Err())
	t.Cleanup(func() { require.NoError(t, h.Redis.Do(context.Background(), "ACL", "DELUSER", aclUser).Err()) })
	denied := redis.NewClient(&redis.Options{Addr: h.Redis.Options().Addr, Username: aclUser, Password: password})
	t.Cleanup(func() { require.NoError(t, denied.Close()) })
	blocked, err := authcore.New(authcore.Config{
		Schema: f.engine.Schema(), Keys: authcore.KeysConfig{VerifyOnly: true},
		Token:        authcore.TokenConfig{Issuer: "https://proof-store.example", IssuedAudiences: []string{"proof-store"}, ExpectedAudiences: []string{"proof-store"}},
		Registration: authcore.RegistrationConfig{Verification: authkit.RegistrationVerificationNone},
	}, authcore.Deps{Postgres: h.sharedPool(), Redis: denied})
	require.NoError(t, err)
	verify.WithDPoP(blocked.ClaimDPoPProof, target)(cp.DelegatedVerifier())
	status, headers := request(external)
	require.Equal(t, 503, status)
	require.Empty(t, headers.Get("WWW-Authenticate"))
	verify.WithDPoP(cp.Core().ClaimDPoPProof, target)(cp.DelegatedVerifier())
	status, _ = request(external)
	require.Equal(t, 200, status)

	_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.merchants SET status='deleted',deleted_at=now() WHERE id=$1`, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := h.sharedPool().Exec(context.Background(), `UPDATE openrails.merchants SET status='active',deleted_at=NULL WHERE id=$1`, dbtest.TestMerchantID.UUID())
		require.NoError(t, err)
	})
	status, _ = request(external)
	require.Equal(t, 403, status, "a valid issuer cannot authorize a retired merchant")
}
