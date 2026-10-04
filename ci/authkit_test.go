//go:build e2e && integration

package ci_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	"github.com/open-rails/openrails"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hostauth"
	"github.com/open-rails/openrails/internal/operator"
)

// A staff member's permission checks act as their own sign-in: a session
// revoked after its token was minted is a 401 at the next check, not an
// outage. Merchant credentials are minted and revoked through AuthKit, by the
// user themselves or, for a non-user credential, by the system after
// OpenRails' no-escalation check.
func TestMerchantCredentialsActAsTheirSession(t *testing.T) {
	f := newFixture(t)
	cp := f.attachControlPlane(t, reserving())
	ctx := t.Context()
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	owner := newAccount(t, cp)
	shop := uniqueName("staff")
	_, err = cp.ProvisionMerchant(ctx, billing.ProvisionMerchantRequest{Slug: shop, OwnerUserID: owner.ID})
	require.NoError(t, err)
	session := authtest.SignIn(t, cp.AuthKit(), owner).AccessToken

	w := call(t, handler, session, http.MethodGet, "/v1/merchant/team", shop, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"role":"owner"`)
	mint := func(token, selector, role string) (int, map[string]any) {
		w := call(t, handler, token, http.MethodPost, "/v1/merchant/api-keys", selector, map[string]string{"name": role + " key", "role": role})
		out := map[string]any{}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&out))
		return w.Code, out
	}
	status, ownerKey := mint(session, shop, "owner")
	require.Equal(t, http.StatusCreated, status, "%v", ownerKey)
	require.True(t, strings.HasPrefix(ownerKey["prefix"].(string), "openrails_st_"))
	status, viewerKey := mint(ownerKey["secret"].(string), "", "viewer")
	require.Equal(t, http.StatusCreated, status, "an owner key mints within its own authority: %v", viewerKey)
	status, body := mint(viewerKey["secret"].(string), "", "viewer")
	require.Equal(t, http.StatusForbidden, status, "a viewer key cannot manage credentials: %v", body)

	findings := func(token string) int {
		return call(t, handler, token, http.MethodGet, "/v1/merchant/findings", "", nil).Code
	}
	require.Equal(t, http.StatusOK, findings(viewerKey["secret"].(string)))
	w = call(t, handler, session, http.MethodDelete, "/v1/merchant/api-keys/"+viewerKey["id"].(string), shop, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, http.StatusUnauthorized, findings(viewerKey["secret"].(string)), "a revoked key authenticates nothing")

	w = call(t, handler, session, http.MethodPost, "/v1/merchant/team/invites", shop, map[string]string{"email": uniqueName("nobody") + "@e2e.test", "role": "viewer"})
	require.Equal(t, http.StatusConflict, w.Code, "self-hosted registration is closed: %s", w.Body.String())

	_, err = cp.AuthKit().RevokeAccountSessions(ctx, iam.UserActor(owner.ID), owner.ID)
	require.NoError(t, err)
	w = call(t, handler, session, http.MethodGet, "/v1/merchant/team", shop, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "credential_revoked")
	require.Equal(t, http.StatusOK, findings(ownerKey["secret"].(string)), "control: the owner's API key is not the revoked session")
}

// newAccount creates an account with a verified email and a password, which
// authtest.SignIn signs in. Names are unique: tests share AuthKit's schema.
func newAccount(t *testing.T, cp *openrails.Client) authtest.User {
	t.Helper()
	name := "a" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	email := name + "@e2e.test"
	u, err := cp.AuthKit().CreateUser(t.Context(), iam.NewUser{Email: email, Username: name, Password: authtest.Password, EmailVerified: true})
	require.NoError(t, err)
	return authtest.User{User: u, Email: email, Password: authtest.Password}
}

// A merchant's own AuthKit deployment signs for it (#259): OpenRails trusts
// the issuer the merchant manifest registers, with a static JWK, under the
// merchant's group, for delegated tokens, its own tokens and service JWTs,
// within the authority of its role there. Nothing else it claims counts.
func TestMerchantIssuerIsTrustedWithinItsGroup(t *testing.T) {
	f := newFixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signer, err := keys.SignerFromKey("merchant-key", key)
	require.NoError(t, err)
	issuer := "https://" + strings.ReplaceAll(f.schema, "_", "-") + ".merchant.e2e.test"
	merchantAuth, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Token.Issuer = issuer }),
		authtest.WithDeps(func(d *authkit.Deps) {
			d.Postgres = f.pool
			d.KeySource = keys.Static{Active: signer, Public: map[string]crypto.PublicKey{signer.KID(): signer.Public()}}
		}))

	cp := f.attachControlPlane(t, nil)
	jwk := keys.PublicJWK(signer.Public(), signer.KID(), "")
	shop := uniqueName("federated")
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(fmt.Sprintf(`version: 1
merchants:
  %s:
    display_name: Federated
    remote_application:
      issuer: %s
      jwks:
        keys:
          - {kty: "%s", kid: "%s", n: "%s", e: "%s"}
`, shop, issuer, jwk.Kty, jwk.Kid, jwk.N, jwk.E)), 0o600))
	graph := engine.Graph(cp)
	require.NoError(t, serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, manifest, nil, ""))
	mid, _, err := cp.ResolveMerchantForGroup(t.Context(), shop)
	require.NoError(t, err)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)

	// A browser's DPoP key; the receiver's proof target is the issuer's origin.
	browser, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	x, y := enc(browser.X.FillBytes(make([]byte, 32))), enc(browser.Y.FillBytes(make([]byte, 32)))
	thumb := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`))
	proof := func(method, path, token string) string {
		ath := sha256.Sum256([]byte(token))
		p := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"htm": method, "htu": "http://127.0.0.1" + path, "ath": enc(ath[:]), "iat": time.Now().Unix(), "jti": uuid.NewString()})
		p.Header["typ"], p.Header["jwk"] = "dpop+jwt", map[string]string{"kty": "EC", "crv": "P-256", "x": x, "y": y}
		signed, err := p.SignedString(browser)
		require.NoError(t, err)
		return signed
	}
	const path = "/v1/merchant/findings"
	send := func(authorization, dpop string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", authorization)
		if dpop != "" {
			r.Header.Set("DPoP", dpop)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	delegated := func(perms ...string) string {
		token, err := merchantAuth.MintDelegatedAccessToken(t.Context(), iam.SystemActor(), iam.DelegatedAccess{
			Subject: uuid.NewString(), Audiences: []string{"openrails"}, Permissions: perms, JWKThumbprint: enc(thumb[:]),
		})
		require.NoError(t, err)
		return token.Value
	}

	token := delegated(billing.MerchantRepairAlertsRead)
	first := proof(http.MethodGet, path, token)
	w := send("DPoP "+token, first)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, http.StatusUnauthorized, send("DPoP "+token, first).Code, "a proof is spent once")
	require.Equal(t, http.StatusUnauthorized, send("Bearer "+token, "").Code, "a bound token needs its proof")
	outside := delegated(billing.RootMerchantsRead)
	require.Equal(t, http.StatusUnauthorized, send("DPoP "+outside, proof(http.MethodGet, path, outside)).Code, "a delegation beyond the stored grant is refused")
	var customers int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema, "customers"}.Sanitize()+" WHERE merchant_id = $1", mid.UUID()).Scan(&customers))
	require.Equal(t, 1, customers, "the delegated subject is the merchant's customer")

	service := func(perms ...string) string {
		token, _, err := merchantAuth.MintServiceJWT(t.Context(), iam.ServiceJWT{Subject: "sync", Audiences: []string{"openrails"}, Permissions: perms})
		require.NoError(t, err)
		return "Bearer " + token.Value
	}
	require.Equal(t, http.StatusOK, send(service(billing.MerchantRepairAlertsRead), "").Code)
	require.Equal(t, http.StatusForbidden, send(service("root:*"), "").Code, "a service JWT only narrows its stored grants")

	self := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer, "aud": "openrails", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
	self.Header["typ"], self.Header["kid"] = "remote-application-access+jwt", signer.KID()
	own, err := self.SignedString(key)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, send("Bearer "+own, "").Code, "the application acting as itself")

	stranger, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Token.Issuer = "https://stranger.e2e.test" }),
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }))
	foreign, _, err := stranger.MintServiceJWT(t.Context(), iam.ServiceJWT{Subject: "sync", Audiences: []string{"openrails"}, Permissions: []string{billing.MerchantRepairAlertsRead}})
	require.NoError(t, err)
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, send("Bearer "+foreign.Value, "").Code, "an unregistered issuer is trusted for nothing")

	// Disabling the application out of band applies without OpenRails
	// reloading anything. A merchant keeps an owner: a person takes over
	// before its only owner, the application, is disabled.
	app, err := cp.AuthKit().RemoteApplication(t.Context(), iam.AppByIssuer(issuer))
	require.NoError(t, err)
	_, err = cp.AuthKit().SetGroupRole(t.Context(), iam.SystemActor(), iam.GroupByID(app.GroupID), iam.UserSubject(newAccount(t, cp).ID), operator.MerchantType.OwnerRole())
	require.NoError(t, err)
	app.Enabled = false
	_, err = cp.AuthKit().UpsertRemoteApplication(t.Context(), iam.SystemActor(), iam.GroupByID(app.GroupID), app)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return send(service(billing.MerchantRepairAlertsRead), "").Code != http.StatusOK }, 20*time.Second, 200*time.Millisecond)
}

// Operator paths: Bootstrap binds a registered merchant to a group keyed by
// the merchant and mints its first deployment key once; customer portal groups
// are keyed by the customer; billing reads users through AuthKit; the example
// authority manifest parses against OpenRails' roles.
func TestControlPlaneOperatorPaths(t *testing.T) {
	f := newFixture(t)
	cp := f.attachControlPlane(t, nil)
	ctx := t.Context()
	require.NoError(t, engine.Graph(cp).Runtime.InitRiver(ctx), "bind job producers, as the standalone boot does")
	admin := newAccount(t, cp)

	slug := uniqueName("unbound")
	var mid string
	require.NoError(t, f.pool.QueryRow(ctx, "INSERT INTO "+pgx.Identifier{f.schema, "merchants"}.Sanitize()+" (slug) VALUES ($1) RETURNING id::text", slug).Scan(&mid))
	res, err := operator.RunBootstrap(ctx, engine.Graph(cp), operator.BootstrapOptions{BootstrapMerchantSlug: slug, InitialAdminUserID: admin.ID, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.True(t, res.MerchantGroupCreated)
	require.Equal(t, mid, res.BootstrapMerchantGroupID, "the group is keyed by the merchant")
	require.True(t, res.APIKeyMinted)
	again, err := operator.RunBootstrap(ctx, engine.Graph(cp), operator.BootstrapOptions{BootstrapMerchantSlug: slug, InitialAdminUserID: admin.ID, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.False(t, again.MerchantGroupCreated || again.APIKeyMinted, "a rerun changes nothing")
	roles, err := cp.AuthKit().GroupRoles(ctx, iam.GroupByID(mid), []iam.Subject{iam.UserSubject(admin.ID)})
	require.NoError(t, err)
	require.Equal(t, "owner", roles[iam.UserSubject(admin.ID)].Name())
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, call(t, handler, res.APIKeySecret, http.MethodGet, "/v1/merchant/findings", "", nil).Code, "the deployment key acts for its merchant")

	customer := newAccount(t, cp)
	for range 2 {
		group, err := cp.EnsureCustomerPermissionGroup(ctx, customer.ID, customer.ID)
		require.NoError(t, err)
		require.Equal(t, customer.ID, group)
	}
	roles, err = cp.AuthKit().GroupRoles(ctx, operator.CustomerGroup(customer.ID), []iam.Subject{iam.UserSubject(customer.ID)})
	require.NoError(t, err)
	require.Equal(t, "owner", roles[iam.UserSubject(customer.ID)].Name())

	directory := hostauth.NewDirectory(cp.AuthKit())
	payer := newAccount(t, cp)
	username, email, ok, err := directory.EmailIdentity(ctx, payer.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{payer.Username, payer.Email}, []string{username, email})
	renamed := "r" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err = cp.AuthKit().UpdateUser(ctx, iam.UserActor(payer.ID), payer.ID, iam.UserUpdate{Username: &renamed})
	require.NoError(t, err)
	id, err := directory.GetUserIDByUsername(ctx, payer.Username)
	require.NoError(t, err)
	require.Equal(t, payer.ID, id, "a former username still resolves")
	results, err := cp.AuthKit().DeleteUsers(ctx, iam.UserActor(payer.ID), []string{payer.ID})
	require.NoError(t, err)
	require.NoError(t, results[0].Err)
	_, _, ok, err = directory.EmailIdentity(ctx, payer.ID)
	require.NoError(t, err)
	require.False(t, ok, "billing mails no deleted account")

	raw, err := os.ReadFile(filepath.Join("..", "config", "bootstrap.example.yaml"))
	require.NoError(t, err)
	manifest, err := authkit.ParseBootstrapManifestYAML(raw)
	require.NoError(t, err)
	_, err = cp.AuthKit().ApplyBootstrapManifest(ctx, manifest, iam.BootstrapOptions{DryRun: true})
	require.NoError(t, err)
}
