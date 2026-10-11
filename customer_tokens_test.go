package openrails

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// opaqueSigner hides the key's concrete type, as a KMS or HSM signer does.
type opaqueSigner struct{ crypto.Signer }

func assertionSigners(t *testing.T) map[string]crypto.Signer {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	out := map[string]crypto.Signer{"RS256": rsaKey, "EdDSA": edKey}
	for alg, curve := range map[string]elliptic.Curve{"ES256": elliptic.P256(), "ES384": elliptic.P384(), "ES512": elliptic.P521()} {
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		require.NoError(t, err)
		out[alg] = key
	}
	return out
}

func assertionHandle(t *testing.T, tokenURL string, signer crypto.Signer) *Client {
	t.Helper()
	handle, err := NewRemoteMerchant("https://billing.example", RemoteCredential{APIKey: "k", Assertion: &AssertionKey{
		Issuer: "https://shop.example", KeyID: "k1", Signer: opaqueSigner{signer}, TokenURL: tokenURL,
	}})
	require.NoError(t, err)
	return handle
}

// The assertion is the RFC 7523 one the server's AuthKit redeems: signed by
// the registered key in its family's algorithm, for the token endpoint, two
// minutes, with a fresh id.
func TestCustomerAssertionClaims(t *testing.T) {
	const tokenURL = "https://billing.example/oauth2/token"
	customer := billing.CustomerID(uuid.New())
	for alg, signer := range assertionSigners(t) {
		handle := assertionHandle(t, tokenURL, signer)
		first, err := handle.CustomerAssertion(t.Context(), billing.CustomerTokenParams{CustomerID: customer, Email: " buyer@shop.example ", EmailVerified: true})
		require.NoError(t, err, alg)
		claims := jwt.MapClaims{}
		token, err := jwt.NewParser(jwt.WithValidMethods([]string{alg}), jwt.WithAudience(tokenURL), jwt.WithIssuer("https://shop.example"), jwt.WithIssuedAt(), jwt.WithExpirationRequired()).
			ParseWithClaims(first, claims, func(*jwt.Token) (any, error) { return signer.Public(), nil })
		require.NoError(t, err, alg)
		require.Equal(t, "k1", token.Header["kid"], alg)
		require.Equal(t, customer.String(), claims["sub"], alg)
		require.Equal(t, "buyer@shop.example", claims["email"], alg)
		require.Equal(t, true, claims["email_verified"], alg)
		require.EqualValues(t, 120, claims["exp"].(float64)-claims["iat"].(float64), alg)
		require.Len(t, claims["jti"], 32, alg)

		second, err := handle.CustomerAssertion(t.Context(), billing.CustomerTokenParams{CustomerID: customer})
		require.NoError(t, err, alg)
		bare := jwt.MapClaims{}
		_, _, err = jwt.NewParser().ParseUnverified(second, bare)
		require.NoError(t, err, alg)
		require.NotEqual(t, claims["jti"], bare["jti"], "each assertion redeems once")
		require.NotContains(t, bare, "email", alg)
		require.NotContains(t, bare, "email_verified", alg)
	}

	handle := assertionHandle(t, tokenURL, assertionSigners(t)["ES256"])
	_, err := handle.CustomerAssertion(t.Context(), billing.CustomerTokenParams{})
	require.ErrorIs(t, err, billing.ErrInvalid, "a customer is required")
}

// CustomerToken redeems its assertion at the credential's token endpoint and
// answers the access token; the endpoint's refusal keeps its status.
func TestCustomerTokenRedeemsTheAssertion(t *testing.T) {
	signer := assertionSigners(t)["ES256"]
	customer := billing.CustomerID(uuid.New())
	var tokenURL string
	refuse := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/oauth2/token", r.URL.Path)
		require.Empty(t, r.Header.Get("Authorization"), "the assertion is the grant: no client credential")
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		require.NoError(t, r.ParseForm())
		require.Equal(t, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "scope": {"openrails:self"}, "assertion": r.PostForm["assertion"]}, r.PostForm)
		claims := jwt.MapClaims{}
		_, err := jwt.NewParser(jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience(tokenURL)).
			ParseWithClaims(r.PostForm.Get("assertion"), claims, func(*jwt.Token) (any, error) { return signer.Public(), nil })
		require.NoError(t, err)
		require.Equal(t, customer.String(), claims["sub"])
		w.Header().Set("Content-Type", "application/json")
		if refuse {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"the assertion was already used"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at-1","token_type":"Bearer","expires_in":900,"scope":"openrails:self"}`))
	}))
	defer server.Close()
	tokenURL = server.URL + "/oauth2/token"
	handle := assertionHandle(t, tokenURL, signer)

	before := time.Now()
	token, err := handle.CustomerToken(t.Context(), billing.CustomerTokenParams{CustomerID: customer})
	require.NoError(t, err)
	require.Equal(t, "at-1", token.AccessToken)
	require.Equal(t, "Bearer", token.TokenType)
	require.WithinDuration(t, before.Add(15*time.Minute), token.ExpiresAt, 5*time.Second)

	refuse = true
	_, err = handle.CustomerToken(t.Context(), billing.CustomerTokenParams{CustomerID: customer})
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusBadRequest, refusal.Status)
	require.Empty(t, refusal.Code, "the token endpoint's errors are not OpenRails codes")
	require.Contains(t, refusal.Message, "invalid_grant: the assertion was already used")
}

// Without an assertion key there is nothing to sign with, and an engine's
// handle never mints: its host's AuthKit does.
func TestCustomerTokenNeedsAnAssertionKey(t *testing.T) {
	params := billing.CustomerTokenParams{CustomerID: billing.CustomerID(uuid.New())}
	remote, err := NewRemoteMerchant("https://billing.example", RemoteCredential{APIKey: "k"})
	require.NoError(t, err)
	inprocess, err := (&Engine{}).Merchant(testMerchant)
	require.NoError(t, err)
	for want, handle := range map[string]*Client{"RemoteCredential.Assertion": remote, "its own AuthKit": inprocess} {
		_, err := handle.CustomerAssertion(t.Context(), params)
		require.ErrorContains(t, err, want)
		_, err = handle.CustomerToken(t.Context(), params)
		require.ErrorContains(t, err, want)
	}
}
