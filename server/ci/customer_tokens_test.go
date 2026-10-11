//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// A merchant's backend mints its customers' tokens with its registered key:
// the token acts as that customer of that merchant alone, and each assertion
// redeems once.
func TestCustomerTokenHelper(t *testing.T) {
	f := newFixture(t)
	srv := f.resourceServer(t)
	a, b := provision(t, srv, uniqueName("tokens-a")), provision(t, srv, uniqueName("tokens-b"))
	backend := newIssuerKey(t, "https://backend-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	trust(t, srv, a, backend.app(t, "viewer", nil))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	api := record(t, handler)
	// The server's AuthKit names itself http://127.0.0.1/<schema>: the client
	// reaches that origin at the listener.
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, api.Listener.Addr().String())
	}}}
	t.Cleanup(client.CloseIdleConnections)
	tokenURL := rsOrigin + "/" + f.schema + iam.OAuthTokenPath
	keyA := merchantKey(t, srv, a, "owner").Secret
	remote, err := openrails.NewRemote(rsOrigin, openrails.RemoteOptions{
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{a: {
			APIKey:    keyA,
			Assertion: &openrails.AssertionKey{Issuer: backend.iss, KeyID: backend.kid, Signer: backend.key, TokenURL: tokenURL},
		}},
		HTTPClient: client, Timeout: 30 * time.Second,
	})
	require.NoError(t, err)
	shop, err := remote.Merchant(a)
	require.NoError(t, err)

	customer := billing.CustomerID(uuid.New())
	before := time.Now()
	token, err := shop.CustomerToken(t.Context(), billing.CustomerTokenParams{CustomerID: customer, Email: "buyer@backend.e2e.test", EmailVerified: true})
	require.NoError(t, err)
	require.Equal(t, "Bearer", token.TokenType)
	require.True(t, token.ExpiresAt.After(before) && token.ExpiresAt.Before(before.Add(time.Hour)), "expires at %s", token.ExpiresAt)
	entitlements := func(selector string) []billing.CustomerEntitlement {
		t.Helper()
		w := serve(handler, rsRequest{path: "/v1/me/entitlements", authorization: "Bearer " + token.AccessToken, selector: selector})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var held struct {
			Data []billing.CustomerEntitlement `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &held))
		return held.Data
	}
	// Mint first: a customer a staff call made before its first token is
	// native, and the issuer pin still refuses the backend's token for it.
	require.Empty(t, entitlements(""))

	// The same person then holds one entitlement at each merchant.
	for key, h := range map[string]*openrails.Client{"at-a": handle(t, srv, a), "at-b": handle(t, srv, b)} {
		product, err := h.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{key}})
		require.NoError(t, err)
		_, err = h.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}})
		require.NoError(t, err)
	}
	require.Equal(t, []billing.CustomerEntitlement{{CustomerID: customer, Entitlement: "at-a"}}, entitlements(""), "that customer of merchant A")
	// The token is bound to A: naming merchant B gives it nothing of B's.
	w := serve(handler, rsRequest{path: "/v1/me/entitlements", authorization: "Bearer " + token.AccessToken, selector: "id:" + b.String()})
	require.NotContains(t, w.Body.String(), "at-b")
	w = serve(handler, rsRequest{path: "/v1/admin/products", authorization: "Bearer " + token.AccessToken})
	require.NotEqual(t, http.StatusOK, w.Code, "a customer token is no staff credential")

	// An assertion the backend hands its frontend redeems once.
	assertion, err := shop.CustomerAssertion(t.Context(), billing.CustomerTokenParams{CustomerID: customer})
	require.NoError(t, err)
	redeem := func() *httptest.ResponseRecorder {
		form := "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Ajwt-bearer&scope=" + billing.ScopeSelf + "&assertion=" + assertion
		r := httptest.NewRequest(http.MethodPost, "/"+f.schema+iam.OAuthTokenPath, strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://app.e2e.test")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	require.Equal(t, http.StatusOK, redeem().Code)
	require.Equal(t, http.StatusBadRequest, redeem().Code, "an assertion is spent once")

	// No assertion key, no customer token: an API key alone, and the engine's
	// own handle, whose host mints with its AuthKit.
	plain, err := openrails.NewRemoteMerchant(api.URL, openrails.RemoteCredential{APIKey: keyA})
	require.NoError(t, err)
	for name, h := range map[string]*openrails.Client{"no assertion key": plain, "in process": handle(t, srv, a)} {
		_, err := h.CustomerToken(t.Context(), billing.CustomerTokenParams{CustomerID: customer})
		require.Error(t, err, name)
		_, err = h.CustomerAssertion(t.Context(), billing.CustomerTokenParams{CustomerID: customer})
		require.Error(t, err, name)
	}
}
