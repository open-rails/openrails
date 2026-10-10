//go:build e2e && integration

package ci_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/staffperm"
	"github.com/open-rails/openrails/server"
)

// resourceID is the server's resource identifier (RFC 8707): its tokens'
// aud. rsOrigin is where clients reach it.
const (
	resourceID = "https://openrails.e2e.test"
	rsOrigin   = "http://127.0.0.1"
)

// issuerKey is a trusted issuer's signing key, pinned in AuthKit.
type issuerKey struct {
	iss string
	kid string
	key *rsa.PrivateKey
}

func newIssuerKey(t *testing.T, iss string) issuerKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return issuerKey{iss: iss, kid: "k-" + uuid.NewString()[:8], key: key}
}

func (k issuerKey) pinned(t *testing.T) []iam.RemoteApplicationKey {
	der, err := x509.MarshalPKIXPublicKey(&k.key.PublicKey)
	require.NoError(t, err)
	return []iam.RemoteApplicationKey{{KID: k.kid, PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}
}

// mint signs an RFC 9068 access token; edit adjusts its claims.
func (k issuerKey) mint(t *testing.T, edit func(jwt.MapClaims)) string {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": k.iss, "aud": resourceID, "sub": "user-" + uuid.NewString()[:8], "client_id": "admin-ui",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": uuid.NewString(), "auth_time": now.Unix(),
		"scope": billing.ScopeMerchant, "permissions": []string{staffperm.BillingRead},
	}
	if edit != nil {
		edit(claims)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["typ"], token.Header["kid"] = "at+jwt", k.kid
	signed, err := token.SignedString(k.key)
	require.NoError(t, err)
	return signed
}

// app is k's issuer as an AuthKit remote application holding the merchant
// role role, with roles its tokens carry mapped to merchant roles.
func (k issuerKey) app(t *testing.T, role string, roleMap map[string]string) iam.RemoteApplication {
	r, ok := server.MerchantRole(role)
	require.True(t, ok, role)
	mapped := map[string]iam.Role{}
	for claim, name := range roleMap {
		mapped[claim], ok = server.MerchantRole(name)
		require.True(t, ok, name)
	}
	return iam.RemoteApplication{Issuer: k.iss, Mode: iam.RemoteApplicationModeStatic, PublicKeys: k.pinned(t), Enabled: true, Role: r, RoleMap: mapped}
}

// trust declares apps the trusted issuers of merchant mid's group.
func trust(t *testing.T, srv *server.Server, mid billing.MerchantID, apps ...iam.RemoteApplication) {
	t.Helper()
	require.NoError(t, srv.AuthKit().DeclareRemoteApplications(t.Context(), iam.GroupByID(mid.String()), apps))
}

type rsRequest struct {
	method, path, authorization, selector, origin, body, idempotencyKey string
}

func serve(handler http.Handler, q rsRequest) *httptest.ResponseRecorder {
	method := q.method
	if method == "" {
		method = http.MethodGet
	}
	r := httptest.NewRequest(method, q.path, strings.NewReader(q.body))
	if q.body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for name, value := range map[string]string{"Authorization": q.authorization, "OpenRails-Merchant": q.selector, "Origin": q.origin, "Idempotency-Key": q.idempotencyKey} {
		if value != "" {
			r.Header.Set(name, value)
		}
	}
	if method == http.MethodOptions {
		r.Header.Set("Access-Control-Request-Method", http.MethodGet)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body.Error.Code
}

// resourceServer is a server that accepts access tokens for resourceID.
func (f *fixture) resourceServer(t *testing.T) *server.Server {
	return f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
	})
}

func provision(t *testing.T, srv *server.Server, slug string) billing.MerchantID {
	owner := newAccount(t, srv)
	p, err := srv.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: slug, OwnerUserID: owner.ID})
	require.NoError(t, err)
	return p.MerchantID
}

// A trusted issuer's at+jwt reaches the admin API through AuthKit: its
// permissions (or mapped roles) within its application's role and the
// token's scopes, at the merchant whose group trusts it and no other.
// OpenRails names no credential of its own; a refusal is generic.
func TestTrustedIssuerTokens(t *testing.T) {
	f := newFixture(t)
	host := newIssuerKey(t, "https://host-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	other := newIssuerKey(t, "https://other-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	stranger := newIssuerKey(t, "https://stranger.e2e.test")
	srv := f.resourceServer(t)
	shop, rival := uniqueName("rs-shop"), uniqueName("rs-rival")
	shopID, rivalID := provision(t, srv, shop), provision(t, srv, rival)
	trust(t, srv, shopID, host.app(t, "support", map[string]string{"billing-admins": "owner"}))
	trust(t, srv, rivalID, other.app(t, "owner", nil))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	const findings, psps = "/v1/admin/findings", "/v1/admin/psps"
	bearer := func(token string) string { return "Bearer " + token }

	t.Run("accepted", func(t *testing.T) {
		w := serve(handler, rsRequest{path: findings, authorization: bearer(host.mint(t, nil))})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		all := host.mint(t, func(c jwt.MapClaims) { c["permissions"] = []string{"merchant:*"} })
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: findings, authorization: bearer(all)}).Code)
		w = serve(handler, rsRequest{path: psps, authorization: bearer(all)})
		require.Equal(t, http.StatusForbidden, w.Code, "its application's role, support, caps merchant:*")
		require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))

		machine := host.mint(t, func(c jwt.MapClaims) { c["sub"], c["client_id"] = "billing-sync", "billing-sync" })
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: findings, authorization: bearer(machine)}).Code, "a client acting for itself")

		admins := host.mint(t, func(c jwt.MapClaims) { delete(c, "permissions"); c["roles"] = []string{"billing-admins"} })
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: findings, authorization: bearer(admins)}).Code, "a role its tokens carry maps to a merchant role")
		none := host.mint(t, func(c jwt.MapClaims) { delete(c, "permissions") })
		require.Equal(t, http.StatusForbidden, serve(handler, rsRequest{path: findings, authorization: bearer(none)}).Code, "no permissions, no access")

		named := serve(handler, rsRequest{path: findings, authorization: bearer(host.mint(t, nil)), selector: shop})
		require.Equal(t, http.StatusOK, named.Code, "its own merchant, named")
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: psps, authorization: bearer(other.mint(t, func(c jwt.MapClaims) { c["permissions"] = []string{"merchant:*"} }))}).Code, "an owner application")
	})

	t.Run("refused", func(t *testing.T) {
		for name, tc := range map[string]struct {
			token, selector string
			status          int
			code            string
		}{
			"unknown issuer":   {stranger.mint(t, nil), "", http.StatusUnauthorized, billing.CodeAuthenticationRequired},
			"wrong audience":   {host.mint(t, func(c jwt.MapClaims) { c["aud"] = "https://elsewhere.e2e.test" }), "", http.StatusUnauthorized, billing.CodeAuthenticationRequired},
			"expired":          {host.mint(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() }), "", http.StatusUnauthorized, billing.CodeCredentialExpired},
			"forged signature": {issuerKey{iss: host.iss, kid: host.kid, key: other.key}.mint(t, nil), "", http.StatusUnauthorized, billing.CodeAuthenticationRequired},
			"another merchant": {host.mint(t, nil), rival, http.StatusConflict, billing.CodeMerchantBindingMismatch},
			"another issuer's": {other.mint(t, nil), shop, http.StatusConflict, billing.CodeMerchantBindingMismatch},
			"customer scope":   {host.mint(t, func(c jwt.MapClaims) { c["scope"] = billing.ScopeSelf }), "", http.StatusForbidden, billing.CodePermissionRequired},
		} {
			w := serve(handler, rsRequest{path: findings, authorization: bearer(tc.token), selector: tc.selector})
			require.Equal(t, tc.status, w.Code, "%s: %s", name, w.Body.String())
			require.Equal(t, tc.code, errorCode(t, w), name)
			if tc.status == http.StatusUnauthorized {
				require.NotEmpty(t, w.Header().Get("WWW-Authenticate"), "%s: AuthKit's challenge passes through", name)
			}
		}
	})

	t.Run("customer", func(t *testing.T) {
		const me = "/v1/me/entitlements"
		customer := func(edit func(jwt.MapClaims)) string {
			return host.mint(t, func(c jwt.MapClaims) {
				c["sub"], c["scope"] = uuid.NewString(), billing.ScopeSelf
				delete(c, "permissions")
				if edit != nil {
					edit(c)
				}
			})
		}
		w := serve(handler, rsRequest{path: me, authorization: bearer(customer(nil)), origin: "https://shop.e2e.test"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))

		session := authtest.SignIn(t, srv.AuthKit(), newAccount(t, srv)).AccessToken
		for name, tc := range map[string]struct {
			authorization, selector string
			status                  int
			code                    string
		}{
			"the server's own user":   {bearer(session), shop, http.StatusConflict, billing.CodeMerchantBindingMismatch},
			"a client for itself":     {bearer(customer(func(c jwt.MapClaims) { c["sub"], c["client_id"] = "billing-sync", "billing-sync" })), "", http.StatusForbidden, billing.CodeInvokerScopedPrincipal},
			"no UUID subject":         {bearer(customer(func(c jwt.MapClaims) { c["sub"] = "user-7" })), "", http.StatusUnauthorized, billing.CodeAuthenticationRequired},
			"another merchant":        {bearer(customer(nil)), rival, http.StatusConflict, billing.CodeMerchantBindingMismatch},
			"an untrusted issuer":     {bearer(stranger.mint(t, func(c jwt.MapClaims) { c["scope"] = billing.ScopeSelf })), shop, http.StatusUnauthorized, billing.CodeAuthenticationRequired},
			"no credential, selected": {"", shop, http.StatusUnauthorized, billing.CodeAuthenticationRequired},
		} {
			w := serve(handler, rsRequest{path: me, authorization: tc.authorization, selector: tc.selector})
			require.Equal(t, tc.status, w.Code, "%s: %s", name, w.Body.String())
			require.Equal(t, tc.code, errorCode(t, w), name)
		}
	})

	t.Run("cors", func(t *testing.T) {
		for _, path := range []string{findings, "/v1/me/entitlements"} {
			w := serve(handler, rsRequest{method: http.MethodOptions, path: path, origin: "https://admin.host.e2e.test"})
			require.Equal(t, http.StatusNoContent, w.Code)
			require.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
			require.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"), "bearer credentials, never cookies")
			auth, ok := srv.AuthKit().Authenticator().(interface{ AllowedHeaders() []string })
			require.True(t, ok)
			for _, name := range auth.AllowedHeaders() {
				require.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), name, "AuthKit's credential headers")
			}
		}
	})
}

// A customer is its issuer's subject (OIDC Core §5.7): a customer made by
// one trusted issuer's credential is never reached by another's with the
// same sub, and a customer of the server's own (NULL issuer: made through
// the Go API) by no trusted issuer. Contacts come from the customer's own
// issuer's directory in AuthKit.
func TestCustomersArePinnedToTheirIssuer(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	a := newIssuerKey(t, "https://a-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	b := newIssuerKey(t, "https://b-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	srv := f.resourceServer(t)
	shop := uniqueName("pinned")
	shopID := provision(t, srv, shop)
	trust(t, srv, shopID, a.app(t, "viewer", nil), b.app(t, "viewer", nil))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	const me = "/v1/me/entitlements"
	customer := func(k issuerKey, sub string, edit func(jwt.MapClaims)) string {
		return "Bearer " + k.mint(t, func(c jwt.MapClaims) {
			c["sub"], c["scope"] = sub, billing.ScopeSelf
			delete(c, "permissions")
			if edit != nil {
				edit(c)
			}
		})
	}

	sub := uuid.NewString()
	w := serve(handler, rsRequest{path: me, authorization: customer(a, sub, func(c jwt.MapClaims) {
		c["email"], c["email_verified"], c["name"] = "ada@a.e2e.test", true, "Ada"
	})})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = serve(handler, rsRequest{path: me, authorization: customer(b, sub, nil)})
	require.Equal(t, http.StatusConflict, w.Code, "issuer B presents issuer A's customer's sub: %s", w.Body.String())
	require.Equal(t, billing.CodeMerchantBindingMismatch, errorCode(t, w))
	require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: me, authorization: customer(a, sub, nil)}).Code, "issuer A still is")

	native := newAccount(t, srv)
	verifyEmail(t, srv, native.ID)
	merchantClient, err := srv.Client().With(openrails.WithMerchantID(shopID))
	require.NoError(t, err)
	_, err = merchantClient.UpdateCustomer(ctx, billing.CustomerID(uuid.MustParse(native.ID)), billing.UpdateCustomerParams{})
	require.NoError(t, err, "the host's Go API makes its own user a customer")
	w = serve(handler, rsRequest{path: me, authorization: customer(a, native.ID, nil)})
	require.Equal(t, http.StatusConflict, w.Code, "a trusted issuer never reaches the host's own customer: %s", w.Body.String())

	contact := func(id string) *billing.CustomerContact {
		c, err := merchantClient.GetCustomer(ctx, billing.CustomerID(uuid.MustParse(id)))
		require.NoError(t, err)
		return c.Contact
	}
	got := contact(sub)
	require.NotNil(t, got, "issuer A's directory in AuthKit")
	require.Equal(t, "ada@a.e2e.test", *got.Email)
	got = contact(native.ID)
	require.NotNil(t, got, "the server's own users' directory")
	require.Equal(t, native.Email, *got.Email)
}

// An AuthKit authorization server is a trusted issuer like any other: a
// user's code-flow token and a worker's client credentials reach the admin
// API within its application's role.
func TestTrustedAuthKitAuthorizationServer(t *testing.T) {
	f := newFixture(t)
	roles := authkit.NewRoles()
	merchant := roles.Persona("merchant")
	merchant.Permission("billing", "read")
	merchant.Permission("billing", "admin")
	admin := roles.Root.Role("admin", merchant.All())
	const console, worker, callback = "console", "billing-worker", "https://admin.host.e2e.test/callback"
	secret := strings.Repeat("s", 48)
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }),
		authtest.WithConfig(func(c *authkit.Config) {
			c.Roles = roles
			c.AuthorizationServer = authkit.AuthorizationServerConfig{
				Resources: []authkit.ResourceServerConfig{{ID: resourceID, Scopes: []string{billing.ScopeMerchant, billing.ScopeSelf}, Permissions: []string{"merchant:*"}}},
				Clients: []authkit.OAuthClientConfig{
					{ID: console, SecretSHA256: authtest.ClientSecretSHA256(secret), RedirectURIs: []string{callback}, Resources: []string{resourceID}, GrantTypes: []authkit.OAuthGrantType{authkit.GrantAuthorizationCode}},
					{ID: worker, SecretSHA256: authtest.ClientSecretSHA256(secret), Resources: []string{resourceID},
						Permissions: []string{staffperm.BillingRead}, GrantTypes: []authkit.OAuthGrantType{authkit.GrantClientCredentials}},
				},
			}
		}))
	res, err := as.HTTPClient().Get(as.URL + iam.JWKSPath)
	require.NoError(t, err)
	var set struct {
		Keys []iam.JWK `json:"keys"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&set))
	require.NoError(t, res.Body.Close())
	var pinned []iam.RemoteApplicationKey
	for _, k := range set.Keys {
		pinned = append(pinned, iam.RemoteApplicationKey{KID: k.Kid, JWK: &k})
	}

	srv := f.resourceServer(t)
	shopID := provision(t, srv, uniqueName("rs-authkit"))
	owner, _ := server.MerchantRole("owner")
	trust(t, srv, shopID, iam.RemoteApplication{Issuer: as.URL, Mode: iam.RemoteApplicationModeStatic, PublicKeys: pinned, Enabled: true, Role: owner})
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	const findings, psps = "/v1/admin/findings", "/v1/admin/psps"

	user := authtest.NewUser(t, as.Client)
	authtest.GrantRole(t, as.Client, iam.RootGroup(), iam.UserSubject(user.ID), admin)
	tokens := as.Authorize(t, user, authtest.CodeFlow{ClientID: console, ClientSecret: secret, RedirectURI: callback, Resource: resourceID, Scopes: []string{billing.ScopeMerchant}})
	require.Equal(t, "Bearer", tokens.TokenType)
	require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: psps, authorization: "Bearer " + tokens.AccessToken}).Code, "a user the issuer grants merchant:*")
	nobody := as.Authorize(t, authtest.NewUser(t, as.Client), authtest.CodeFlow{ClientID: console, ClientSecret: secret, RedirectURI: callback, Resource: resourceID, Scopes: []string{billing.ScopeMerchant}})
	w := serve(handler, rsRequest{path: findings, authorization: "Bearer " + nobody.AccessToken})
	require.Equal(t, http.StatusForbidden, w.Code, "a user the issuer grants nothing")
	require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))

	machine := as.ClientCredentials(t, worker, secret, resourceID, []string{billing.ScopeMerchant}, nil)
	require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: findings, authorization: "Bearer " + machine.AccessToken}).Code)
	w = serve(handler, rsRequest{path: psps, authorization: "Bearer " + machine.AccessToken})
	require.Equal(t, http.StatusForbidden, w.Code, "only the client's own grants")
	require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))
}

// A trusted application without an authorization server mints its customer
// a token at the server's own token endpoint (RFC 7523 §2.1): its frontend
// redeems the application's assertion for a token that acts for the user at
// the application's merchant, once.
func TestTrustedApplicationAssertion(t *testing.T) {
	f := newFixture(t)
	app := newIssuerKey(t, "https://app-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	srv := f.resourceServer(t)
	shopID := provision(t, srv, uniqueName("assert"))
	trust(t, srv, shopID, app.app(t, "viewer", nil))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	tokenPath := "/" + f.schema + iam.OAuthTokenPath
	user := uuid.NewString()
	assertion := func() string {
		now := time.Now()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": app.iss, "sub": user, "aud": "http://127.0.0.1" + tokenPath, "jti": uuid.NewString(),
			"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "email": "buyer@app.e2e.test", "email_verified": true,
		})
		token.Header["kid"] = app.kid
		signed, err := token.SignedString(app.key)
		require.NoError(t, err)
		return signed
	}
	redeem := func(assertion string) *httptest.ResponseRecorder {
		form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}, "scope": {billing.ScopeSelf}}
		r := httptest.NewRequest(http.MethodPost, tokenPath, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://app.e2e.test")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	spent := assertion()
	w := redeem(spent)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var tokens struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tokens))
	require.Equal(t, "Bearer", tokens.TokenType, "bearer unless the frontend proves a key")
	w = serve(handler, rsRequest{path: "/v1/me/entitlements", authorization: "Bearer " + tokens.AccessToken})
	require.Equal(t, http.StatusOK, w.Code, "the application's user is its merchant's customer: %s", w.Body.String())
	require.Equal(t, http.StatusBadRequest, redeem(spent).Code, "an assertion is spent once")
}
