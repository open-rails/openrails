//go:build e2e && integration

package ci_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/staffperm"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/operator"
)

const resourceID = "https://openrails.e2e.test"

// issuerKey is a trusted issuer's signing key, pinned in OpenRails' config.
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
	return k.mintAs(t, "at+jwt", edit)
}

// mintAs signs the same claims as another kind of token.
func (k issuerKey) mintAs(t *testing.T, typ string, edit func(jwt.MapClaims)) string {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": k.iss, "aud": resourceID, "sub": "user-" + uuid.NewString()[:8], "client_id": "admin-ui",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": uuid.NewString(), "auth_time": now.Unix(),
		"scope": "openrails:merchant", "permissions": []string{staffperm.BillingRead},
	}
	if edit != nil {
		edit(claims)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["typ"], token.Header["kid"] = typ, k.kid
	signed, err := token.SignedString(k.key)
	require.NoError(t, err)
	return signed
}

// browserKey is a DPoP key; proofs target the control plane's origin.
type browserKey struct {
	key       *ecdsa.PrivateKey
	x, y, jkt string
}

func newBrowserKey(t *testing.T) browserKey {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	x, y := enc(key.X.FillBytes(make([]byte, 32))), enc(key.Y.FillBytes(make([]byte, 32)))
	thumb := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`))
	return browserKey{key: key, x: x, y: y, jkt: enc(thumb[:])}
}

func (b browserKey) proof(t *testing.T, method, path, token, nonce string) string {
	enc := base64.RawURLEncoding.EncodeToString
	ath := sha256.Sum256([]byte(token))
	claims := jwt.MapClaims{"htm": method, "htu": rsOrigin + path, "ath": enc(ath[:]), "iat": time.Now().Unix(), "jti": uuid.NewString()}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	p := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	p.Header["typ"], p.Header["jwk"] = "dpop+jwt", map[string]string{"kty": "EC", "crv": "P-256", "x": b.x, "y": b.y}
	signed, err := p.SignedString(b.key)
	require.NoError(t, err)
	return signed
}

type rsRequest struct {
	method, path, authorization, dpop, selector, origin, body, idempotencyKey string
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
	for name, value := range map[string]string{"Authorization": q.authorization, "DPoP": q.dpop, "OpenRails-Merchant": q.selector, "Origin": q.origin, "Idempotency-Key": q.idempotencyKey} {
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

func provision(t *testing.T, cp *server.Server, slug string) {
	owner := newAccount(t, cp)
	_, err := cp.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: slug, OwnerUserID: owner.ID})
	require.NoError(t, err)
}

// A trusted issuer's at+jwt reaches the admin API with the token's
// permissions within the issuer's ceiling, on the merchants the issuer is
// trusted for only; DPoP-bound tokens need a fresh, nonce-carrying proof;
// every other issuer, audience or lifetime is refused.
func TestResourceServerAcceptsTrustedIssuerTokens(t *testing.T) {
	f := newFixture(t)
	host := newIssuerKey(t, "https://host-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	other := newIssuerKey(t, "https://other-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	stranger := newIssuerKey(t, "https://stranger.e2e.test")
	shop, rival := uniqueName("rs-shop"), uniqueName("rs-rival")
	const adminOrigin = "https://admin.host.e2e.test"
	cp := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.ResourceServer = &server.ResourceServerConfig{
			Identifier:   resourceID,
			DPoPNonceKey: strings.Repeat("n", 32),
			TrustedIssuers: []server.TrustedIssuerConfig{
				{
					Name: "host", Issuer: host.iss, Keys: host.pinned(t), Merchants: []string{shop},
					Permissions:    []string{staffperm.BillingRead, staffperm.CatalogManage, staffperm.ConfigManage},
					AllowedOrigins: []string{adminOrigin},
					GroupRoles:     map[string]string{"billing-admins": "owner"},
				},
				{Name: "other", Issuer: other.iss, Keys: other.pinned(t), Merchants: []string{rival}, Permissions: []string{"merchant:*"}},
			},
		}
	})
	provision(t, cp, shop)
	provision(t, cp, rival)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	const findings, psps, catalog = "/v1/admin/findings", "/v1/admin/psps", "/v1/admin/catalog/revision"
	cancel := "/v1/admin/subscriptions/" + billing.SubscriptionID(uuid.New()).String() + "/cancel"
	bearer := func(token string) string { return "Bearer " + token }

	t.Run("accepted", func(t *testing.T) {
		w := serve(handler, rsRequest{path: findings, authorization: bearer(host.mint(t, nil))})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		all := host.mint(t, func(c jwt.MapClaims) { c["permissions"] = []string{"merchant:*"} })
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: psps, authorization: bearer(all)}).Code, "merchant:* within the ceiling")
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: catalog, authorization: bearer(all)}).Code)
		w = serve(handler, rsRequest{method: http.MethodPost, path: cancel, authorization: bearer(all), body: "{}"})
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Equal(t, billing.CodePermissionRequired, errorCode(t, w), "the ceiling caps merchant:*: no staff writes")

		machine := host.mint(t, func(c jwt.MapClaims) { c["sub"], c["client_id"] = "billing-sync", "billing-sync" })
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: findings, authorization: bearer(machine)}).Code, "a client acting for itself")

		admins := host.mint(t, func(c jwt.MapClaims) { delete(c, "permissions"); c["roles"] = []string{"billing-admins"} })
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: findings, authorization: bearer(admins)}).Code, "a group role maps to a merchant role")
		none := host.mint(t, func(c jwt.MapClaims) { delete(c, "permissions") })
		require.Equal(t, http.StatusForbidden, serve(handler, rsRequest{path: findings, authorization: bearer(none)}).Code, "no permissions, no access")

		named := serve(handler, rsRequest{path: findings, authorization: bearer(host.mint(t, nil)), selector: shop})
		require.Equal(t, http.StatusOK, named.Code, "the issuer's merchant, named")
	})

	t.Run("refused", func(t *testing.T) {
		for name, tc := range map[string]struct {
			token, selector string
			status          int
			code            string
		}{
			"unknown issuer":     {stranger.mint(t, nil), "", http.StatusUnauthorized, billing.CodeAccessTokenIssuerUnknown},
			"wrong audience":     {host.mint(t, func(c jwt.MapClaims) { c["aud"] = "https://elsewhere.e2e.test" }), "", http.StatusUnauthorized, billing.CodeAccessTokenInvalid},
			"expired":            {host.mint(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() }), "", http.StatusUnauthorized, billing.CodeCredentialExpired},
			"not yet valid":      {host.mint(t, func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(10 * time.Minute).Unix() }), "", http.StatusUnauthorized, billing.CodeAccessTokenInvalid},
			"forged signature":   {issuerKey{iss: host.iss, kid: host.kid, key: other.key}.mint(t, nil), "", http.StatusUnauthorized, billing.CodeAccessTokenInvalid},
			"merchant not bound": {host.mint(t, nil), rival, http.StatusForbidden, billing.CodeAccessTokenMerchantNotBound},
			"another issuer's":   {other.mint(t, nil), shop, http.StatusForbidden, billing.CodeAccessTokenMerchantNotBound},
			"customer scope":     {host.mint(t, func(c jwt.MapClaims) { c["scope"] = billing.ScopeSelf }), "", http.StatusForbidden, billing.CodeInsufficientScope},
		} {
			w := serve(handler, rsRequest{path: findings, authorization: bearer(tc.token), selector: tc.selector})
			require.Equal(t, tc.status, w.Code, "%s: %s", name, w.Body.String())
			require.Equal(t, tc.code, errorCode(t, w), name)
		}
	})

	t.Run("dpop", func(t *testing.T) {
		browser := newBrowserKey(t)
		bound := host.mint(t, func(c jwt.MapClaims) { c["cnf"] = map[string]string{"jkt": browser.jkt} })
		w := serve(handler, rsRequest{path: findings, authorization: bearer(bound)})
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Equal(t, billing.CodeSenderProofRequired, errorCode(t, w), "a bound token needs its proof")

		w = serve(handler, rsRequest{path: findings, authorization: "DPoP " + bound, dpop: browser.proof(t, http.MethodGet, findings, bound, "")})
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Equal(t, billing.CodeDPoPNonceRequired, errorCode(t, w))
		nonce := w.Header().Get("DPoP-Nonce")
		require.NotEmpty(t, nonce, "the challenge carries a nonce")
		require.Contains(t, w.Header().Get("WWW-Authenticate"), "use_dpop_nonce")

		proof := browser.proof(t, http.MethodGet, findings, bound, nonce)
		w = serve(handler, rsRequest{path: findings, authorization: "DPoP " + bound, dpop: proof})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		w = serve(handler, rsRequest{path: findings, authorization: "DPoP " + bound, dpop: proof})
		require.Equal(t, http.StatusUnauthorized, w.Code, "a proof is spent once")

		elsewhere := browser.proof(t, http.MethodGet, psps, bound, nonce)
		require.Equal(t, http.StatusUnauthorized, serve(handler, rsRequest{path: findings, authorization: "DPoP " + bound, dpop: elsewhere}).Code, "a proof names its request")
	})

	t.Run("merchants", func(t *testing.T) {
		w := serve(userMerchants(cp), rsRequest{path: "/hosted/merchants", authorization: bearer(host.mint(t, func(c jwt.MapClaims) { delete(c, "permissions"); c["roles"] = []string{"billing-admins"} }))})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		list := merchantList(t, w)
		require.Len(t, list, 1)
		require.Equal(t, shop, list[0].Slug)
		require.Equal(t, "custom", list[0].Role, "owner grants capped by the ceiling are no named role")
		require.ElementsMatch(t, []string{staffperm.BillingRead, staffperm.CatalogManage, staffperm.ConfigManage}, list[0].Permissions)

		w = serve(userMerchants(cp), rsRequest{path: "/hosted/merchants", authorization: bearer(host.mint(t, func(c jwt.MapClaims) { delete(c, "permissions") }))})
		require.Equal(t, http.StatusOK, w.Code)
		require.Empty(t, merchantList(t, w), "a token granting nothing lists nothing")

		w = serve(userMerchants(cp), rsRequest{path: "/hosted/merchants", authorization: bearer(stranger.mint(t, nil))})
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Equal(t, billing.CodeAccessTokenIssuerUnknown, errorCode(t, w))
		w = serve(userMerchants(cp), rsRequest{path: "/hosted/merchants", authorization: bearer(host.mint(t, func(c jwt.MapClaims) { c["scope"] = billing.ScopeSelf }))})
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Equal(t, billing.CodeInsufficientScope, errorCode(t, w))
	})

	t.Run("customer", func(t *testing.T) {
		const me = "/v1/me/entitlements"
		browser := newBrowserKey(t)
		customer := func(edit func(jwt.MapClaims)) string {
			return host.mint(t, func(c jwt.MapClaims) {
				c["sub"], c["scope"], c["cnf"] = uuid.NewString(), billing.ScopeSelf, map[string]string{"jkt": browser.jkt}
				delete(c, "permissions")
				if edit != nil {
					edit(c)
				}
			})
		}
		token := customer(nil)
		// A shop's page calls /v1/me across origins: it can read the nonce
		// the challenge carries, and retry with it.
		w := serve(handler, rsRequest{path: me, authorization: "DPoP " + token, dpop: browser.proof(t, http.MethodGet, me, token, ""), origin: "https://shop.e2e.test"})
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		require.Equal(t, billing.CodeDPoPNonceRequired, errorCode(t, w))
		require.NotEmpty(t, w.Header().Get("DPoP-Nonce"))
		require.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
		require.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "DPoP-Nonce")
		w = staticDPoPServe(t, handler, browser, token, rsRequest{path: me})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var customers int
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema, "customers"}.Sanitize()+" WHERE merchant_id = (SELECT id FROM "+pgx.Identifier{f.schema, "merchants"}.Sanitize()+" WHERE slug = $1)", shop).Scan(&customers))
		require.Equal(t, 1, customers, "the token's user is the shop's customer")

		for name, tc := range map[string]struct {
			token, scheme string
			status        int
			code          string
		}{
			"unbound":          {customer(func(c jwt.MapClaims) { delete(c, "cnf") }), "Bearer", http.StatusUnauthorized, billing.CodeSenderProofRequired},
			"merchant scope":   {customer(func(c jwt.MapClaims) { c["scope"] = billing.ScopeMerchant }), "DPoP", http.StatusForbidden, billing.CodeInsufficientScope},
			"machine":          {customer(func(c jwt.MapClaims) { c["sub"], c["client_id"] = "billing-sync", "billing-sync" }), "DPoP", http.StatusUnauthorized, billing.CodeAccessTokenInvalid},
			"no UUID subject":  {customer(func(c jwt.MapClaims) { c["sub"] = "user-7" }), "DPoP", http.StatusUnauthorized, billing.CodeAccessTokenInvalid},
			"unknown issuer":   {stranger.mint(t, func(c jwt.MapClaims) { c["scope"] = billing.ScopeSelf }), "Bearer", http.StatusUnauthorized, billing.CodeAccessTokenIssuerUnknown},
			"merchant unbound": {customer(nil), "DPoP-rival", http.StatusForbidden, billing.CodeAccessTokenMerchantNotBound},
		} {
			q := rsRequest{path: me, authorization: "Bearer " + tc.token}
			var w *httptest.ResponseRecorder
			switch tc.scheme {
			case "DPoP":
				w = staticDPoPServe(t, handler, browser, tc.token, q)
			case "DPoP-rival":
				q.selector = rival
				w = staticDPoPServe(t, handler, browser, tc.token, q)
			default:
				w = serve(handler, q)
			}
			require.Equal(t, tc.status, w.Code, "%s: %s", name, w.Body.String())
			require.Equal(t, tc.code, errorCode(t, w), name)
		}
		legacy := host.mintAs(t, "delegated-access+jwt", func(c jwt.MapClaims) { c["delegated_sub"] = uuid.NewString(); delete(c, "sub") })
		w = serve(handler, rsRequest{path: me, authorization: "Bearer " + legacy})
		require.Equal(t, http.StatusUnauthorized, w.Code, "a delegated token is no customer credential")
		require.Equal(t, billing.CodeAccessTokenInvalid, errorCode(t, w))
	})

	t.Run("cors", func(t *testing.T) {
		w := serve(handler, rsRequest{method: http.MethodOptions, path: findings, origin: adminOrigin})
		require.Equal(t, http.StatusNoContent, w.Code)
		require.Equal(t, adminOrigin, w.Header().Get("Access-Control-Allow-Origin"))
		require.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "DPoP")
		require.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "DPoP-Nonce")
		require.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"), "bearer and DPoP headers, never cookies")

		w = serve(handler, rsRequest{path: findings, authorization: bearer(host.mint(t, nil)), origin: adminOrigin})
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, adminOrigin, w.Header().Get("Access-Control-Allow-Origin"))

		w = serve(handler, rsRequest{method: http.MethodOptions, path: findings, origin: "https://evil.e2e.test"})
		require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"), "an undeclared origin gets no CORS")
	})
}

// An AuthKit authorization server is a trusted issuer like any other: the
// console's code flow, the host admin UI's token exchange and a worker's
// client credentials each mint DPoP-bound or bearer at+jwt tokens that
// OpenRails authorizes from alone, within the issuer's ceiling.
func TestResourceServerTrustsAnAuthKitAuthorizationServer(t *testing.T) {
	f := newFixture(t)
	roles := authkit.NewRoles()
	merchant := roles.Persona("merchant")
	merchant.Permission("operations", "read")
	merchant.Permission("psps", "read")
	admin := roles.Root.Role("admin", merchant.All())
	const console, adminUI, worker, callback = "console", "admin-ui", "billing-worker", "https://admin.host.e2e.test/callback"
	const adminOrigin = "https://admin.host.e2e.test"
	workerSecret := strings.Repeat("s", 48)
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }),
		authtest.WithConfig(func(c *authkit.Config) {
			c.Roles = roles
			c.AuthorizationServer = authkit.AuthorizationServerConfig{
				Resources: []authkit.ResourceServerConfig{{ID: resourceID, Scopes: []string{billing.ScopeMerchant, billing.ScopeSelf}, Permissions: []string{"merchant:*"}}},
				Clients: []authkit.OAuthClientConfig{
					{ID: console, RedirectURIs: []string{callback}, Resources: []string{resourceID}, GrantTypes: []authkit.OAuthGrantType{authkit.GrantAuthorizationCode, authkit.GrantRefreshToken}},
					{ID: adminUI, Origins: []string{adminOrigin}, Resources: []string{resourceID}, GrantTypes: []authkit.OAuthGrantType{authkit.GrantTokenExchange}},
					{ID: worker, SecretSHA256: authtest.ClientSecretSHA256(workerSecret), Resources: []string{resourceID},
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

	shop := uniqueName("rs-authkit")
	cp := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.ResourceServer = &server.ResourceServerConfig{
			Identifier: resourceID, DPoPNonceKey: strings.Repeat("n", 32),
			TrustedIssuers: []server.TrustedIssuerConfig{{
				Name: "authkit", Issuer: as.URL, Keys: pinned, Merchants: []string{shop},
				Permissions: []string{"merchant:*"}, AllowedOrigins: []string{adminOrigin},
			}},
		}
	})
	provision(t, cp, shop)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	const findings, psps = "/v1/admin/findings", "/v1/admin/psps"
	flow := authtest.CodeFlow{ClientID: console, RedirectURI: callback, Resource: resourceID, Scopes: []string{billing.ScopeMerchant}}
	owner := authtest.NewUser(t, as.Client)
	authtest.GrantRole(t, as.Client, iam.RootGroup(), iam.UserSubject(owner.ID), admin)

	t.Run("console", func(t *testing.T) {
		tokens := as.Authorize(t, owner, flow)
		require.Equal(t, "DPoP", tokens.TokenType)
		w := serve(handler, rsRequest{path: findings, authorization: "Bearer " + tokens.AccessToken})
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Equal(t, billing.CodeSenderProofRequired, errorCode(t, w), "a DPoP-bound token is no bearer token")

		w = serve(handler, rsRequest{path: findings, authorization: "DPoP " + tokens.AccessToken, dpop: tokens.DPoP.Proof(t, http.MethodGet, rsOrigin+findings, tokens.AccessToken, "")})
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Equal(t, billing.CodeDPoPNonceRequired, errorCode(t, w))
		require.Equal(t, http.StatusOK, dpopServe(t, handler, tokens, rsRequest{path: findings}).Code)

		renewed := as.Refresh(t, console, "", tokens)
		require.NotEqual(t, tokens.AccessToken, renewed.AccessToken)
		require.Equal(t, http.StatusOK, dpopServe(t, handler, renewed, rsRequest{path: psps}).Code, "a refreshed token")

		nobody := as.Authorize(t, authtest.NewUser(t, as.Client), flow)
		w = dpopServe(t, handler, nobody, rsRequest{path: findings})
		require.Equal(t, http.StatusForbidden, w.Code, "a user the issuer grants nothing")
		require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))

		self := as.Authorize(t, owner, authtest.CodeFlow{ClientID: console, RedirectURI: callback, Resource: resourceID, Scopes: []string{billing.ScopeSelf}})
		w = dpopServe(t, handler, self, rsRequest{path: findings})
		require.Equal(t, http.StatusForbidden, w.Code, "a customer token is not a merchant token")
		require.Equal(t, billing.CodeInsufficientScope, errorCode(t, w))
		require.Contains(t, w.Header().Get("WWW-Authenticate"), `scope="openrails:merchant"`)
		require.Equal(t, http.StatusOK, dpopServe(t, handler, self, rsRequest{path: "/v1/me/entitlements"}).Code, "the user's own billing")

		w = dpopServe(t, userMerchants(cp), renewed, rsRequest{path: "/hosted/merchants"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		list := merchantList(t, w)
		require.Len(t, list, 1)
		require.Equal(t, shop, list[0].Slug)
		require.Equal(t, "owner", list[0].Role)
		w = dpopServe(t, userMerchants(cp), nobody, rsRequest{path: "/hosted/merchants"})
		require.Equal(t, http.StatusOK, w.Code)
		require.Empty(t, merchantList(t, w))
	})

	t.Run("host admin UI", func(t *testing.T) {
		preflight := serve(handler, rsRequest{method: http.MethodOptions, path: findings, origin: adminOrigin})
		require.Equal(t, http.StatusNoContent, preflight.Code)
		require.Equal(t, adminOrigin, preflight.Header().Get("Access-Control-Allow-Origin"))

		tokens := as.Exchange(t, authtest.TokenExchange{ClientID: adminUI, SubjectToken: authtest.SignIn(t, as.Client, owner).AccessToken, Resource: resourceID, Scopes: []string{billing.ScopeMerchant}})
		w := dpopServe(t, handler, tokens, rsRequest{path: findings, origin: adminOrigin})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, adminOrigin, w.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("machine", func(t *testing.T) {
		tokens := as.ClientCredentials(t, worker, workerSecret, resourceID, []string{billing.ScopeMerchant}, nil)
		w := serve(handler, rsRequest{path: findings, authorization: "Bearer " + tokens.AccessToken})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		w = serve(handler, rsRequest{path: psps, authorization: "Bearer " + tokens.AccessToken})
		require.Equal(t, http.StatusForbidden, w.Code, "only the client's own grants")
		require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))
	})
}

// staticDPoPServe calls q with token bound to browser, retrying once with
// the server's nonce.
func staticDPoPServe(t *testing.T, handler http.Handler, browser browserKey, token string, q rsRequest) *httptest.ResponseRecorder {
	t.Helper()
	method := q.method
	if method == "" {
		method = http.MethodGet
	}
	q.authorization, q.dpop = "DPoP "+token, browser.proof(t, method, q.path, token, "")
	w := serve(handler, q)
	if nonce := w.Header().Get("DPoP-Nonce"); w.Code == http.StatusUnauthorized && nonce != "" {
		q.dpop = browser.proof(t, method, q.path, token, nonce)
		w = serve(handler, q)
	}
	return w
}

func merchantList(t *testing.T, w *httptest.ResponseRecorder) []billing.UserMerchant {
	var page billing.ListPage[billing.UserMerchant]
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page), w.Body.String())
	return page.Items
}

// rsOrigin is the control plane's public origin, the URL DPoP proofs sign.
const rsOrigin = "http://127.0.0.1"

// dpopServe calls q with tokens' DPoP key, retrying once with the server's
// nonce as a client does.
func dpopServe(t *testing.T, handler http.Handler, tokens authtest.OAuthTokens, q rsRequest) *httptest.ResponseRecorder {
	t.Helper()
	require.NotNil(t, tokens.DPoP, "a DPoP-bound token")
	method := q.method
	if method == "" {
		method = http.MethodGet
	}
	q.authorization = "DPoP " + tokens.AccessToken
	q.dpop = tokens.DPoP.Proof(t, method, rsOrigin+q.path, tokens.AccessToken, "")
	w := serve(handler, q)
	if nonce := w.Header().Get("DPoP-Nonce"); w.Code == http.StatusUnauthorized && nonce != "" {
		q.dpop = tokens.DPoP.Proof(t, method, rsOrigin+q.path, tokens.AccessToken, nonce)
		w = serve(handler, q)
	}
	return w
}

// A merchant's registered remote application (its manifest's
// remote_application) is a trusted issuer bound to that merchant, within the
// authority of its role there, read live. Its old token kinds are refused.
func TestResourceServerTrustsRegisteredIssuers(t *testing.T) {
	f := newFixture(t)
	app := newIssuerKey(t, "https://"+strings.ReplaceAll(f.schema, "_", "-")+".merchant.e2e.test")
	cp := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.ResourceServer = &server.ResourceServerConfig{Identifier: resourceID, DPoPNonceKey: strings.Repeat("n", 32)}
	})
	jwk := keys.PublicJWK(&app.key.PublicKey, app.kid, "")
	shop := uniqueName("registered")
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(fmt.Sprintf(`version: 1
merchants:
  %s:
    display_name: Registered
    remote_application:
      issuer: %s
      jwks:
        keys:
          - {kty: "%s", kid: "%s", n: "%s", e: "%s"}
`, shop, app.iss, jwk.Kty, jwk.Kid, jwk.N, jwk.E)), 0o600))
	graph, plane := operator.Of(cp)
	require.NoError(t, serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, plane, manifest, nil, ""))
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	const findings = "/v1/admin/findings"
	call := func(token string) *httptest.ResponseRecorder {
		return serve(handler, rsRequest{path: findings, authorization: "Bearer " + token})
	}

	require.Equal(t, http.StatusOK, call(app.mint(t, nil)).Code)
	require.Equal(t, http.StatusOK, call(app.mint(t, func(c jwt.MapClaims) { c["permissions"] = []string{"merchant:*"} })).Code, "an owner application")
	w := call(app.mint(t, func(c jwt.MapClaims) { c["permissions"] = []string{"root:*"} }))
	require.Equal(t, http.StatusForbidden, w.Code, "nothing beyond its authority")
	require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))
	require.Equal(t, http.StatusOK, call(app.mint(t, func(c jwt.MapClaims) { c["sub"], c["client_id"] = "sync", "sync" })).Code, "the application acting for itself")

	w = serve(userMerchants(cp), rsRequest{path: "/hosted/merchants", authorization: "Bearer " + app.mint(t, func(c jwt.MapClaims) { c["permissions"] = []string{"merchant:*"} })})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := merchantList(t, w)
	require.Len(t, list, 1)
	require.Equal(t, shop, list[0].Slug)
	require.Equal(t, "owner", list[0].Role)

	for typ, edit := range map[string]func(jwt.MapClaims){
		"delegated-access+jwt":          func(c jwt.MapClaims) { c["delegated_sub"] = uuid.NewString(); delete(c, "sub") },
		"remote-application-access+jwt": func(c jwt.MapClaims) { delete(c, "sub") },
		"access+jwt":                    nil,
	} {
		w := call(app.mintAs(t, typ, edit))
		require.Equal(t, http.StatusUnauthorized, w.Code, "%s is refused: %s", typ, w.Body.String())
	}

	// Disabling the application out of band applies on the next request. A
	// merchant keeps an owner: a person takes over before its only owner,
	// the application, is disabled.
	registered, err := cp.AuthKit().RemoteApplication(t.Context(), iam.AppByIssuer(app.iss))
	require.NoError(t, err)
	_, err = cp.AuthKit().SetGroupRole(t.Context(), iam.SystemIdentity(), iam.GroupByID(registered.GroupID), iam.UserSubject(newAccount(t, cp).ID), operator.MerchantType.OwnerRole())
	require.NoError(t, err)
	registered.Enabled = false
	_, err = cp.AuthKit().UpsertRemoteApplication(t.Context(), iam.SystemIdentity(), iam.GroupByID(registered.GroupID), registered)
	require.NoError(t, err)
	w = call(app.mint(t, nil))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, billing.CodeAccessTokenIssuerUnknown, errorCode(t, w))
}

// An owner grants a merchant role by email; a user of an issuer trusted for
// the merchant accepts it with that verified email and then holds the role,
// within the issuer's ceiling, until it is revoked.
func TestResourceServerFederatedGrants(t *testing.T) {
	f := newFixture(t)
	host := newIssuerKey(t, "https://grants-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	other := newIssuerKey(t, "https://grants-other-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	shop, rival := uniqueName("fg-shop"), uniqueName("fg-rival")
	cp := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.ResourceServer = &server.ResourceServerConfig{
			Identifier: resourceID, DPoPNonceKey: strings.Repeat("n", 32),
			TrustedIssuers: []server.TrustedIssuerConfig{
				{Name: "host", Issuer: host.iss, Keys: host.pinned(t), Merchants: []string{shop}, Permissions: []string{"merchant:*"}},
				{Name: "other", Issuer: other.iss, Keys: other.pinned(t), Merchants: []string{rival}, Permissions: []string{"merchant:*"}},
			},
		}
	})
	provision(t, cp, shop)
	provision(t, cp, rival)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	type body = map[string]any
	send := func(method, path, token string, payload any) *httptest.ResponseRecorder {
		var raw []byte
		if payload != nil {
			raw, err = json.Marshal(payload)
			require.NoError(t, err)
		}
		r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		r.Header.Set("Authorization", "Bearer "+token)
		if payload != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	owner := host.mint(t, func(c jwt.MapClaims) { c["permissions"] = []string{"merchant:*"} })
	staffSub := "staff-" + uuid.NewString()[:8]
	staff := func(edit func(jwt.MapClaims)) string {
		return host.mint(t, func(c jwt.MapClaims) {
			c["sub"], c["email"], c["email_verified"] = staffSub, "staff@example.test", true
			delete(c, "permissions")
			if edit != nil {
				edit(c)
			}
		})
	}
	const findings = "/v1/admin/findings"
	mid, _, err := cp.ResolveMerchantForGroup(t.Context(), shop)
	require.NoError(t, err)
	ownerActor := server.CredentialActor([]string{"merchant:*"})
	invite := func(by server.Actor, email, role string) (*billing.FederatedGrant, error) {
		return cp.CreateFederatedGrant(t.Context(), by, mid, billing.CreateFederatedGrantParams{Email: email, Role: role})
	}
	as := func(token string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}

	grant, err := invite(ownerActor, " Staff@Example.test ", "viewer")
	require.NoError(t, err)
	require.Equal(t, "staff@example.test", grant.Email)
	require.Nil(t, grant.Subject, "pending")
	for name, tc := range map[string]struct {
		by          server.Actor
		email, role string
		want        error
	}{
		"duplicate":     {ownerActor, "staff@example.test", "support", server.ErrFederatedGrantExists},
		"bad email":     {ownerActor, "Staff <staff@example.test>", "viewer", server.ErrFederatedGrantInvalidEmail},
		"unknown role":  {ownerActor, "x@example.test", "admin", server.ErrUnknownMerchantRole},
		"beyond caller": {server.CredentialActor([]string{staffperm.MembersManage}), "x@example.test", "owner", server.ErrRoleEscalation},
	} {
		_, err := invite(tc.by, tc.email, tc.role)
		require.ErrorIs(t, err, tc.want, name)
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/merchant/federated-grants"}, {http.MethodPost, "/v1/merchant/federated-grants"}, {http.MethodGet, "/v1/merchants/invites"},
	} {
		require.Equal(t, http.StatusNotFound, send(route.method, route.path, owner, body{}).Code, "%s %s is the hosted product's", route.method, route.path)
	}

	require.Equal(t, http.StatusForbidden, send(http.MethodGet, findings, staff(nil), nil).Code, "no access until accepted")
	pending, err := cp.ListFederatedInvites(t.Context(), as(staff(nil)))
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, grant.ID, pending[0].ID)
	require.Equal(t, shop, pending[0].Merchant.Slug)

	unverified := staff(func(c jwt.MapClaims) { c["email_verified"] = false })
	pending, err = cp.ListFederatedInvites(t.Context(), as(unverified))
	require.NoError(t, err)
	require.Empty(t, pending, "an unverified email is offered nothing")
	_, err = cp.AcceptFederatedGrant(t.Context(), as(unverified), grant.ID)
	require.ErrorIs(t, err, server.ErrFederatedGrantUnverified)
	stranger := other.mint(t, func(c jwt.MapClaims) { c["email"], c["email_verified"] = "staff@example.test", true })
	_, err = cp.AcceptFederatedGrant(t.Context(), as(stranger), grant.ID)
	require.ErrorIs(t, err, server.ErrFederatedGrantNotFound, "an issuer not trusted for the merchant cannot accept its grants")
	_, err = cp.ListFederatedInvites(t.Context(), as("not-a-token"))
	refusal, ok := server.CredentialRefusal(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, http.StatusUnauthorized, refusal.Status)

	reached, err := cp.AcceptFederatedGrant(t.Context(), as(staff(nil)), grant.ID)
	require.NoError(t, err)
	require.Equal(t, shop, reached.Slug)
	require.Equal(t, "viewer", reached.Role)
	_, err = cp.AcceptFederatedGrant(t.Context(), as(staff(nil)), grant.ID)
	require.ErrorIs(t, err, server.ErrFederatedGrantNotFound, "a grant is accepted once")

	require.Equal(t, http.StatusOK, send(http.MethodGet, findings, staff(nil), nil).Code, "the grant's role")
	list, err := cp.ListUserMerchants(t.Context(), as(staff(nil)))
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "viewer", list[0].Role)
	_, err = invite(server.CredentialActor(list[0].Permissions), "y@example.test", "owner")
	require.ErrorIs(t, err, server.ErrRoleEscalation, "a viewer grants no more than it holds")

	roster, err := cp.ListFederatedGrants(t.Context(), mid)
	require.NoError(t, err)
	require.Len(t, roster, 1)
	require.Equal(t, host.iss, *roster[0].Issuer)
	require.Equal(t, staffSub, *roster[0].Subject)

	require.NoError(t, cp.RevokeFederatedGrant(t.Context(), ownerActor, mid, grant.ID))
	require.ErrorIs(t, cp.RevokeFederatedGrant(t.Context(), ownerActor, mid, grant.ID), server.ErrFederatedGrantNotFound)
	require.Equal(t, http.StatusForbidden, send(http.MethodGet, findings, staff(nil), nil).Code, "revoked")
}

// userMerchants is how a hosted product serves a signed-in user's merchants
// on the server's Go API: ListUserMerchants, a refused credential answered
// by CredentialRefusal with its retry headers.
func userMerchants(srv *server.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, err := srv.ListUserMerchants(r.Context(), r)
		if refusal, ok := server.CredentialRefusal(err); ok {
			for name, value := range refusal.Headers {
				w.Header().Set(name, value)
			}
			w.WriteHeader(refusal.Status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": refusal.Code}})
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(billing.ListPage[billing.UserMerchant]{Items: list})
	})
}
