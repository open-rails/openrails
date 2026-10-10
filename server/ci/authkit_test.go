//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/operator"
)

// A staff member's changes to the team and keys act as their own sign-in: a
// session revoked after its token was minted is refused at the next change,
// not an outage. Merchant credentials are minted and revoked through AuthKit,
// by the user themselves or, for a credential carrying its own permissions,
// by the system after OpenRails' no-escalation check. The keys reach the
// admin API with their role.
func TestMerchantCredentialsActAsTheirSession(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, reserving())
	ctx := t.Context()
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	owner := newAccount(t, cp)
	shop := uniqueName("staff")
	provisioned, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: shop, OwnerUserID: owner.ID})
	require.NoError(t, err)
	mid := provisioned.MerchantID
	session := authtest.SignIn(t, cp.AuthKit(), owner).AccessToken
	ownerActor := userActor(t, cp, session)

	team, err := cp.ListMerchantTeam(ctx, mid)
	require.NoError(t, err)
	require.Len(t, team, 1)
	require.Equal(t, "owner", team[0].Role)
	ownerKey, err := cp.CreateMerchantAPIKey(ctx, ownerActor, mid, billing.CreateAPIKeyParams{Name: "owner key", Role: "owner"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(ownerKey.Prefix, "openrails_st_"))
	viewerKey, err := cp.CreateMerchantAPIKey(ctx, server.CredentialActor([]string{server.MerchantRead, server.MerchantWrite, server.MerchantAdmin}), mid, billing.CreateAPIKeyParams{Name: "viewer key", Role: "viewer"})
	require.NoError(t, err, "a credential mints within its own authority")
	_, err = cp.CreateMerchantAPIKey(ctx, server.CredentialActor([]string{server.MerchantRead}), mid, billing.CreateAPIKeyParams{Name: "escalated", Role: "owner"})
	require.ErrorIs(t, err, server.ErrRoleEscalation)
	_, err = cp.CreateMerchantAPIKey(ctx, ownerActor, mid, billing.CreateAPIKeyParams{Name: "x", Role: "admin"})
	require.ErrorIs(t, err, server.ErrUnknownMerchantRole)
	keys, err := cp.ListMerchantAPIKeys(ctx, mid)
	require.NoError(t, err)
	require.Len(t, keys, 2)

	findings := func(token string) int {
		return call(t, handler, token, http.MethodGet, "/v1/admin/findings", "", nil).Code
	}
	require.Equal(t, http.StatusOK, findings(viewerKey.Secret))
	require.Equal(t, http.StatusForbidden, call(t, handler, viewerKey.Secret, http.MethodGet, "/v1/admin/psps", "", nil).Code, "a viewer reads no merchant configuration")
	require.Equal(t, http.StatusOK, call(t, handler, ownerKey.Secret, http.MethodGet, "/v1/admin/psps", "", nil).Code)
	attempt := billing.CheckoutAttemptID(uuid.New()).String()
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/v1/admin/checkout-attempts"},
		{http.MethodGet, "/v1/admin/checkout-attempts/" + attempt},
		{http.MethodPost, "/v1/admin/checkout-attempts/" + attempt + "/confirm"},
		{http.MethodGet, "/v1/merchant/team"},
		{http.MethodPost, "/v1/merchant/api-keys"},
		{http.MethodGet, "/v1/merchants"},
	} {
		w := call(t, handler, ownerKey.Secret, route.method, route.path, "", map[string]any{})
		require.Equal(t, http.StatusNotFound, w.Code, "%s %s is not mounted: %s", route.method, route.path, w.Body.String())
	}
	require.NoError(t, cp.RevokeMerchantAPIKey(ctx, ownerActor, mid, viewerKey.ID))
	require.Equal(t, http.StatusUnauthorized, findings(viewerKey.Secret), "a revoked key authenticates nothing")
	require.ErrorIs(t, cp.RevokeMerchantAPIKey(ctx, ownerActor, mid, viewerKey.ID+"x"), billing.ErrNotFound)

	_, err = cp.InviteMerchantTeamMember(ctx, ownerActor, mid, billing.InviteTeamMemberParams{Email: uniqueName("nobody") + "@e2e.test", Role: "viewer"})
	require.ErrorIs(t, err, server.ErrTeamInvitesDisabled, "self-hosted registration is closed")
	require.False(t, cp.TeamInvitesEnabled())

	_, err = cp.AuthKit().RevokeAccountSessions(ctx, iam.UserIdentity(owner.ID), owner.ID)
	require.NoError(t, err)
	_, err = cp.CreateMerchantAPIKey(ctx, ownerActor, mid, billing.CreateAPIKeyParams{Name: "late", Role: "viewer"})
	require.ErrorIs(t, err, iam.ErrSessionRevoked)
	require.Equal(t, http.StatusOK, findings(ownerKey.Secret), "control: the owner's API key is not the revoked session")
}

// userActor is the server account a bearer token signs in.
func userActor(t *testing.T, srv *server.Server, token string) server.Actor {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	actor, err := srv.UserActor(r)
	require.NoError(t, err)
	return actor
}

// newAccount creates an account with a verified email and a password, which
// authtest.SignIn signs in. Names are unique: tests share AuthKit's schema.
func newAccount(t *testing.T, cp *server.Server) authtest.User {
	t.Helper()
	name := "a" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	email := name + "@e2e.test"
	u, err := cp.AuthKit().CreateUser(t.Context(), iam.NewUser{Email: email, Username: name, Password: authtest.Password, EmailVerified: true})
	require.NoError(t, err)
	return authtest.User{User: u, Email: email, Password: authtest.Password}
}

// Operator paths: Bootstrap binds a registered merchant to a group keyed by
// the merchant and mints its first deployment key once; customer portal groups
// are keyed by the customer; the example authority manifest parses against
// OpenRails' roles.
func TestControlPlaneOperatorPaths(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, nil)
	ctx := t.Context()
	require.NoError(t, engine.Graph(cp.Client()).Runtime.InitRiver(ctx), "bind job producers, as the standalone boot does")
	admin := newAccount(t, cp)

	slug := uniqueName("unbound")
	var mid string
	require.NoError(t, f.pool.QueryRow(ctx, "INSERT INTO "+pgx.Identifier{f.schema, "merchants"}.Sanitize()+" (slug, status) VALUES ($1, 'active') RETURNING id::text", slug).Scan(&mid))
	_, plane := operator.Of(cp)
	res, err := plane.Bootstrap(ctx, controlplane.BootstrapOptions{BootstrapMerchantSlug: slug, InitialAdminUserID: admin.ID, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.True(t, res.MerchantGroupCreated)
	require.Equal(t, mid, res.BootstrapMerchantGroupID, "the group is keyed by the merchant")
	require.True(t, res.APIKeyMinted)
	again, err := plane.Bootstrap(ctx, controlplane.BootstrapOptions{BootstrapMerchantSlug: slug, InitialAdminUserID: admin.ID, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.False(t, again.MerchantGroupCreated || again.APIKeyMinted, "a rerun changes nothing")
	roles, err := cp.AuthKit().GroupRoles(ctx, iam.GroupByID(mid), []iam.Subject{iam.UserSubject(admin.ID)})
	require.NoError(t, err)
	require.Equal(t, "owner", roles[iam.UserSubject(admin.ID)].Name())
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, call(t, handler, res.APIKeySecret, http.MethodGet, "/v1/admin/findings", "", nil).Code, "the deployment key acts for its merchant")

	customer := newAccount(t, cp)
	for range 2 {
		group, err := cp.EnsureCustomerPermissionGroup(ctx, customer.ID, customer.ID)
		require.NoError(t, err)
		require.Equal(t, customer.ID, group)
	}
	roles, err = cp.AuthKit().GroupRoles(ctx, operator.CustomerGroup(customer.ID), []iam.Subject{iam.UserSubject(customer.ID)})
	require.NoError(t, err)
	require.Equal(t, "owner", roles[iam.UserSubject(customer.ID)].Name())

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "bootstrap.example.yaml"))
	require.NoError(t, err)
	manifest, err := authkit.ParseBootstrapManifestYAML(raw)
	require.NoError(t, err)
	_, err = cp.AuthKit().ApplyBootstrapManifest(ctx, manifest, iam.BootstrapOptions{DryRun: true})
	require.NoError(t, err)
}
