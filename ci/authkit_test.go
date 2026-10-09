//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
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
	_, err = cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: shop, OwnerUserID: owner.ID})
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
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
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
	require.NoError(t, f.pool.QueryRow(ctx, "INSERT INTO "+pgx.Identifier{f.schema, "merchants"}.Sanitize()+" (slug, status) VALUES ($1, 'active') RETURNING id::text", slug).Scan(&mid))
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
