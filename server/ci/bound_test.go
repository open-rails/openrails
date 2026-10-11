//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// retiredMerchantHeader is the request header that once named a merchant:
// tests send it to show it names nothing.
const retiredMerchantHeader = "OpenRails" + "-Merchant"

// boundToken is an access token for the server's resource, bound to its DPoP
// key: a person's sign-in exchanged for one merchant through the console's
// client.
type boundToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
	key         *authtest.DPoPKey
}

// tokenPath is the path of the fixture server's token endpoint, and tokenURL
// its URL, the issuer's.
func (f *fixture) tokenPath() string { return "/" + f.schema + iam.OAuthTokenPath }
func (f *fixture) tokenURL() string  { return rsOrigin + f.tokenPath() }

// exchangeFor trades session, a person's access token at the server's own
// AuthKit, for one bound to merchant mid (RFC 8693 naming its group), as the
// admin console does: through server.ConsoleClientID, with a DPoP proof. It
// returns the answer's status and body; a server nonce is answered once.
func (f *fixture) exchangeFor(t *testing.T, handler http.Handler, session string, mid billing.MerchantID) (*boundToken, int, []byte) {
	t.Helper()
	key := authtest.NewDPoPKey(t)
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"client_id":          {server.ConsoleClientID},
		"subject_token":      {session},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"resource":           {resourceID},
		"scope":              {billing.ScopeMerchant},
		"group_id":           {mid.String()},
	}
	nonce := ""
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, f.tokenPath(), strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", rsOrigin)
		r.Header.Set("DPoP", key.Proof(t, http.MethodPost, f.tokenURL(), "", nonce))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if next := w.Header().Get("DPoP-Nonce"); w.Code != http.StatusOK && next != "" && nonce == "" {
			nonce = next
			continue
		}
		if w.Code != http.StatusOK {
			return nil, w.Code, w.Body.Bytes()
		}
		tok := &boundToken{key: key}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), tok))
		return tok, w.Code, w.Body.Bytes()
	}
	t.Fatal("the token endpoint asked for a nonce twice")
	return nil, 0, nil
}

// exchanged is exchangeFor, failing the test on a refusal.
func (f *fixture) exchanged(t *testing.T, handler http.Handler, session string, mid billing.MerchantID) *boundToken {
	t.Helper()
	tok, status, body := f.exchangeFor(t, handler, session, mid)
	require.Equal(t, http.StatusOK, status, string(body))
	return tok
}

// call sends a JSON request with the bound token and a fresh proof; a server
// nonce is answered once. header adds request headers.
func (b *boundToken) call(t *testing.T, handler http.Handler, method, path string, body any, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	target, _, _ := strings.Cut(rsOrigin+path, "?")
	nonce := ""
	for {
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "DPoP "+b.AccessToken)
		r.Header.Set("DPoP", b.key.Proof(t, method, target, b.AccessToken, nonce))
		for i := 0; i+1 < len(header); i += 2 {
			r.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if next := w.Header().Get("DPoP-Nonce"); w.Code == http.StatusUnauthorized && next != "" && nonce == "" &&
			strings.Contains(w.Header().Get("WWW-Authenticate"), "use_dpop_nonce") {
			nonce = next
			continue
		}
		return w
	}
}

// resourced makes the server a resource server for resourceID, reached at
// rsOrigin: its persons act on a merchant through a token exchanged for it.
func resourced(cfg *server.Config, _ *server.Deps) {
	cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
}

// edits applies each edit in turn.
func edits(all ...func(*server.Config, *server.Deps)) func(*server.Config, *server.Deps) {
	return func(cfg *server.Config, deps *server.Deps) {
		for _, edit := range all {
			edit(cfg, deps)
		}
	}
}

// callWith is call with request headers, name then value.
func callWith(t *testing.T, handler http.Handler, token, method, path string, body any, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// provisionOwned provisions merchant slug owned by userID.
func provisionOwned(t *testing.T, srv *server.Server, slug, userID string) billing.MerchantID {
	t.Helper()
	p, err := srv.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: slug, OwnerUserID: userID})
	require.NoError(t, err)
	return p.MerchantID
}

// redeem redeems a merchant backend's RFC 7523 assertion for a customer
// token at the server's token endpoint and returns the answer.
func (f *fixture) redeem(t *testing.T, handler http.Handler, assertion string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}, "scope": {billing.ScopeSelf}}
	r := httptest.NewRequest(http.MethodPost, f.tokenPath(), strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://app.e2e.test")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// The server's resource tokens last 15 minutes unless its AuthKit
// configuration says otherwise: a customer token redeemed from a merchant
// backend's assertion and a console token from the exchange alike.
func TestBoundTokensLastFifteenMinutes(t *testing.T) {
	f := newFixture(t)
	for name, tc := range map[string]struct {
		ttl  time.Duration
		want int64
	}{
		"unset":     {0, 900},
		"5 minutes": {5 * time.Minute, 300},
	} {
		srv := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
			resourced(cfg, deps)
			cfg.Auth.AuthorizationServer.AccessTokenTTL = tc.ttl
		})
		handler, err := standaloneHandler(srv)
		require.NoError(t, err)
		ownerID, session := newOwner(t, srv)
		mid := provisionOwned(t, srv, uniqueName("ttl"), ownerID)
		backend := newIssuerKey(t, "https://ttl-"+uuid.NewString()[:8]+".e2e.test")
		trust(t, srv, mid, backend.app(t, "viewer", nil))

		w := f.redeem(t, handler, backend.assertion(t, f.tokenURL(), uuid.NewString()))
		require.Equal(t, http.StatusOK, w.Code, "%s: %s", name, w.Body.String())
		var customer boundToken
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &customer))
		require.Equal(t, tc.want, customer.ExpiresIn, "%s: the customer token", name)
		require.Equal(t, tc.want, f.exchanged(t, handler, session, mid).ExpiresIn, "%s: the console token", name)
	}
}

// A merchant backend's assertion is short and single use, for the token
// endpoint alone: one expiring more than two minutes ahead, one redeemed a
// second time and one for another audience are each refused.
func TestCustomerAssertionLimits(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, resourced)
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	mid := provision(t, srv, uniqueName("assertions"))
	backend := newIssuerKey(t, "https://limits-"+uuid.NewString()[:8]+".e2e.test")
	trust(t, srv, mid, backend.app(t, "viewer", nil))

	once := backend.assertion(t, f.tokenURL(), uuid.NewString())
	require.Equal(t, http.StatusOK, f.redeem(t, handler, once).Code)
	for name, assertion := range map[string]string{
		"redeemed again":        once,
		"three minutes ahead":   backend.assertion(t, f.tokenURL(), uuid.NewString(), func(c jwt.MapClaims) { c["exp"] = time.Now().Add(3 * time.Minute).Unix() }),
		"for another audience":  backend.assertion(t, resourceID, uuid.NewString()),
		"for the API, not here": backend.assertion(t, rsOrigin+"/v1/me", uuid.NewString()),
	} {
		w := f.redeem(t, handler, assertion)
		require.Equal(t, http.StatusBadRequest, w.Code, "%s: %s", name, w.Body.String())
		require.Contains(t, w.Body.String(), "invalid_grant", name)
	}
}
