//go:build integration

package integrationharness

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authhttp"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/testauth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

// A real device-key login exchanged through AuthKit's real HTTP delegation
// route must retain automation provenance. The authorizer reads the original
// verified claims, never a requested attribute or a user profile lookup.
func TestCustomerDelegationRetainsIssuerVerifiedInteraction(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	gateway := NewFakeNMIGateway(t)
	receiver := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL} }))
	di := receiver.RegisterDelegatedIssuer("interaction-"+uuid.NewString()[:8], dbtest.TestMerchantSlug)
	signer := di.issuer.Signer()
	pub := signer.(jwtkit.PublicKeySigner).PublicKey()
	var sawDevice atomic.Bool
	issuerCore, err := authcore.NewWithKeys(authcore.Config{
		Token:        authcore.TokenConfig{Issuer: di.Issuer, IssuedAudiences: []string{"native-session"}, AccessTokenDuration: time.Hour, RefreshTokenDuration: time.Hour},
		DeviceKeys:   authcore.DeviceKeysConfig{Enabled: true},
		Ephemeral:    authcore.EphemeralConfig{KeyPrefix: "interaction-" + uuid.NewString()[:8]},
		Registration: authcore.RegistrationConfig{AllowMissingSenders: true},
		Delegated:    authcore.DelegatedConfig{Audiences: []string{"openrails"}, AllowDPoP: true},
	}, authcore.Keyset{Active: signer, PublicKeys: map[string]crypto.PublicKey{signer.KID(): pub}}, authcore.Deps{Postgres: h.sharedPool(), Redis: h.Redis, DelegatedAuthorization: func(ctx context.Context, req authkit.DelegationRequest) (authkit.DelegationGrant, error) {
		claims, ok := verify.ClaimsFromContext(ctx)
		if !ok || claims.UserID == "" || claims.UserID != req.UserID {
			return authkit.DelegationGrant{}, authkit.ErrDelegationRefused
		}
		class := billingauth.CredentialClassUserSession
		if claims.DeviceKeyID != "" || claims.TokenType != "" {
			class = billingauth.CredentialClassAutomation
			sawDevice.Store(true)
		}
		return authkit.DelegationGrant{Attributes: map[string]any{billingauth.DelegatedCredentialClassAttribute: class}}, nil
	}})
	require.NoError(t, err)
	t.Cleanup(issuerCore.Close)
	service, err := authhttp.New(issuerCore, authhttp.Config{DirectPeerIP: true, DisableRateLimiting: true})
	require.NoError(t, err)
	t.Cleanup(service.Close)
	mounted, err := authhttp.MountHandler(service, authhttp.MountOptions{})
	require.NoError(t, err)
	issuer := httptest.NewServer(mounted)
	t.Cleanup(issuer.Close)
	user, err := issuerCore.CreateUser(ctx, "provenance-"+uuid.NewString()+"@example.test", "provenance"+uuid.NewString()[:8])
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	deviceID := uuid.NewString()
	_, err = h.sharedPool().Exec(ctx, `INSERT INTO profiles.user_device_keys(id,user_id,public_key) VALUES($1,$2,$3)`, deviceID, user.ID, []byte(public))
	require.NoError(t, err)
	challenge, err := issuerCore.BeginDeviceKeyLogin(ctx, deviceID)
	require.NoError(t, err)
	nonce, err := base64.RawURLEncoding.DecodeString(challenge.Challenge)
	require.NoError(t, err)
	message := append([]byte("authkit.device-key-login/1\x00"), nonce...)
	device, err := issuerCore.FinishDeviceKeyLogin(ctx, challenge.ID, base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, message)))
	require.NoError(t, err)
	session, _, err := issuerCore.MintAccessToken(ctx, user.ID, nil)
	require.NoError(t, err)
	fixture := h.SeedPastDueInvoiceForCustomer(receiver.App().Runtime, dbtest.TestMerchantID, uuid.MustParse(user.ID), "USD", 50_000)
	sender, err := testauth.NewSender()
	require.NoError(t, err)
	exchange := func(parent string) string {
		body := `{"audiences":["openrails"],"requested_grant":{"openrails_credential_class":"user_session"}}`
		r, err := http.NewRequestWithContext(ctx, "POST", issuer.URL+"/api/v1/delegated/token", strings.NewReader(body))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+parent)
		endpoint, err := url.Parse(di.Issuer)
		require.NoError(t, err)
		proof, err := sender.Proof("POST", endpoint.Scheme+"://"+endpoint.Host+"/api/v1/delegated/token", parent)
		require.NoError(t, err)
		r.Header.Set("DPoP", proof)
		response, err := issuer.Client().Do(r)
		require.NoError(t, err)
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Equal(t, 200, response.StatusCode, string(raw))
		var result struct {
			Token string `json:"token"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		return result.Token
	}
	call := func(token string, boundSender *testauth.Sender, method, path, body string) int {
		r, err := http.NewRequestWithContext(ctx, method, receiver.BaseURL+path, strings.NewReader(body))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", uuid.NewString())
		if boundSender != nil {
			target := *r.URL
			target.RawQuery = ""
			proof, err := boundSender.Proof(method, target.String(), token)
			require.NoError(t, err)
			r.Header.Set("Authorization", "DPoP "+token)
			r.Header.Set("DPoP", proof)
		} else {
			require.NoError(t, testauth.Authorize(r, token))
		}
		response, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		return response.StatusCode
	}
	payPath := "/v1/me/invoices/" + fixture.Invoice.String() + "/pay-now"
	encoded, err := json.Marshal(map[string]any{"payment_method_id": openrails.PaymentMethodID(fixture.Method)})
	require.NoError(t, err)
	payBody := string(encoded)
	for _, parent := range []struct {
		token string
		pay   int
	}{{device.AccessToken, 403}, {session, 200}} {
		token := exchange(parent.token)
		require.Equal(t, 200, call(token, sender, "GET", "/v1/me/balance?currency=USD", ""))
		require.Equal(t, parent.pay, call(token, sender, "POST", payPath, payBody))
	}
	require.True(t, sawDevice.Load(), "actual authorizer context retained original device-key provenance")
	require.Equal(t, 1, gateway.SaleAttempts())
	for _, attribute := range []any{nil, "invented-class", 7} {
		attrs := map[string]any{}
		if attribute != nil {
			attrs[billingauth.DelegatedCredentialClassAttribute] = attribute
		}
		token, err := mintDelegatedAccessToken(ctx, signer, authkit.DelegatedAccessParams{Issuer: di.Issuer, Audiences: []string{"openrails"}, DelegatedSubject: user.ID, Attributes: attrs})
		require.NoError(t, err)
		if attribute == nil {
			require.Equal(t, 200, call(token, nil, "GET", "/v1/me/balance?currency=USD", ""))
			require.Equal(t, 403, call(token, nil, "POST", payPath, payBody))
		} else {
			require.Equal(t, 401, call(token, nil, "POST", payPath, payBody))
		}
	}
	token := exchange(device.AccessToken)
	parts := strings.Split(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	raw = []byte(strings.ReplaceAll(string(raw), "automation", "user_session"))
	parts[1] = base64.RawURLEncoding.EncodeToString(raw)
	require.Equal(t, 401, call(strings.Join(parts, "."), sender, "POST", payPath, payBody), "changing the signed class is not an authorization path")
	require.Equal(t, 1, gateway.SaleAttempts())
}
