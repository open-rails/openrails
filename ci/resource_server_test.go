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
	"net/http"
	"net/http/httptest"
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
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": k.iss, "aud": resourceID, "sub": "user-" + uuid.NewString()[:8], "client_id": "admin-ui",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": uuid.NewString(),
		"scope": "openrails:merchant", "permissions": []string{billing.MerchantOperationsRead},
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
	claims := jwt.MapClaims{"htm": method, "htu": "http://127.0.0.1" + path, "ath": enc(ath[:]), "iat": time.Now().Unix(), "jti": uuid.NewString()}
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
	method, path, authorization, dpop, selector, origin string
}

func serve(handler http.Handler, q rsRequest) *httptest.ResponseRecorder {
	method := q.method
	if method == "" {
		method = http.MethodGet
	}
	r := httptest.NewRequest(method, q.path, nil)
	for name, value := range map[string]string{"Authorization": q.authorization, "DPoP": q.dpop, "OpenRails-Merchant": q.selector, "Origin": q.origin} {
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

func provision(t *testing.T, cp *openrails.Client, slug string) {
	owner := newAccount(t, cp)
	_, err := cp.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: slug, OwnerUserID: owner.ID})
	require.NoError(t, err)
}

// A trusted issuer's at+jwt reaches the merchant API with the token's
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
	cp := f.attachControlPlane(t, func(cfg *openrails.Config, _ *openrails.Deps) {
		cfg.ControlPlane.ResourceServer = &openrails.ResourceServerConfig{
			Identifier:   resourceID,
			DPoPNonceKey: strings.Repeat("n", 32),
			TrustedIssuers: []openrails.TrustedIssuerConfig{
				{
					Name: "host", Issuer: host.iss, Keys: host.pinned(t), Merchants: []string{shop},
					Permissions:    []string{billing.MerchantOperationsRead, billing.MerchantPSPsRead},
					AllowedOrigins: []string{adminOrigin},
					GroupRoles:     map[string]string{"billing-viewers": "viewer"},
				},
				{Name: "other", Issuer: other.iss, Keys: other.pinned(t), Merchants: []string{rival}, Permissions: []string{"merchant:*"}},
			},
		}
	})
	provision(t, cp, shop)
	provision(t, cp, rival)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	const findings, psps, catalog = "/v1/merchant/findings", "/v1/merchant/psps", "/v1/merchant/catalog/revision"
	bearer := func(token string) string { return "Bearer " + token }

	t.Run("accepted", func(t *testing.T) {
		w := serve(handler, rsRequest{path: findings, authorization: bearer(host.mint(t, nil))})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		all := host.mint(t, func(c jwt.MapClaims) { c["permissions"] = []string{"merchant:*"} })
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: psps, authorization: bearer(all)}).Code, "merchant:* within the ceiling")
		w = serve(handler, rsRequest{path: catalog, authorization: bearer(all)})
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Equal(t, billing.CodePermissionRequired, errorCode(t, w), "the ceiling caps merchant:*")

		machine := host.mint(t, func(c jwt.MapClaims) { c["sub"], c["client_id"] = "billing-sync", "billing-sync" })
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: findings, authorization: bearer(machine)}).Code, "a client acting for itself")

		viewer := host.mint(t, func(c jwt.MapClaims) { delete(c, "permissions"); c["roles"] = []string{"billing-viewers"} })
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: findings, authorization: bearer(viewer)}).Code, "a group role maps to a merchant role")
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

// An AuthKit authorization server is a trusted issuer like any other: its
// code flow mints the user's root grants within the resource's ceiling, and
// OpenRails authorizes from that token alone.
func TestResourceServerTrustsAnAuthKitAuthorizationServer(t *testing.T) {
	f := newFixture(t)
	roles := authkit.NewRoles()
	merchant := roles.Persona("merchant")
	merchant.Permission("operations", "read")
	merchant.Permission("psps", "read")
	admin := roles.Root.Role("admin", merchant.All())
	const console, callback = "console", "https://admin.host.e2e.test/callback"
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }),
		authtest.WithConfig(func(c *authkit.Config) {
			c.Roles = roles
			c.AuthorizationServer = authkit.AuthorizationServerConfig{
				Resources: []authkit.ResourceServerConfig{{ID: resourceID, Scopes: []string{"openrails:merchant"}, Permissions: []string{"merchant:*"}}},
				Clients:   []authkit.OAuthClientConfig{{ID: console, RedirectURIs: []string{callback}, Resources: []string{resourceID}}},
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
	cp := f.attachControlPlane(t, func(cfg *openrails.Config, _ *openrails.Deps) {
		cfg.ControlPlane.ResourceServer = &openrails.ResourceServerConfig{
			Identifier: resourceID, DPoPNonceKey: strings.Repeat("n", 32),
			TrustedIssuers: []openrails.TrustedIssuerConfig{{
				Name: "authkit", Issuer: as.URL, Keys: pinned, Merchants: []string{shop}, Permissions: []string{"merchant:*"},
			}},
		}
	})
	provision(t, cp, shop)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	flow := authtest.CodeFlow{ClientID: console, RedirectURI: callback, Resource: resourceID, Scopes: []string{"openrails:merchant"}}

	owner := authtest.NewUser(t, as.Client)
	authtest.GrantRole(t, as.Client, iam.RootGroup(), iam.UserSubject(owner.ID), admin)
	tokens := as.Authorize(t, owner, flow)
	w := serve(handler, rsRequest{path: "/v1/merchant/findings", authorization: "Bearer " + tokens.AccessToken})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	nobody := as.Authorize(t, authtest.NewUser(t, as.Client), flow)
	w = serve(handler, rsRequest{path: "/v1/merchant/findings", authorization: "Bearer " + nobody.AccessToken})
	require.Equal(t, http.StatusForbidden, w.Code, "a user the issuer grants nothing")
	require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))
}
