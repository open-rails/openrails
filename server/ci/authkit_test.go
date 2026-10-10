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

// A merchant's API keys are AuthKit's, in its group: each reaches the admin
// API with its role, at its own merchant only, and a revoked key
// authenticates nothing. No OpenRails route manages them.
func TestMerchantAPIKeysAreAuthKits(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	ctx := t.Context()
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	owner := newAccount(t, srv)
	provision := func(name string) billing.MerchantID {
		p, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName(name), OwnerUserID: owner.ID})
		require.NoError(t, err)
		return p.MerchantID
	}
	mid, other := provision("keys"), provision("other")
	ownerKey, viewerKey := merchantKey(t, srv, mid, "owner"), merchantKey(t, srv, mid, "viewer")
	require.True(t, strings.HasPrefix(ownerKey.Secret, "openrails_st_"))

	findings := func(token, selector string) int {
		return call(t, handler, token, http.MethodGet, "/v1/admin/findings", selector, nil).Code
	}
	require.Equal(t, http.StatusOK, findings(viewerKey.Secret, ""), "a key names its merchant")
	require.Equal(t, http.StatusForbidden, call(t, handler, viewerKey.Secret, http.MethodGet, "/v1/admin/psps", "", nil).Code, "a viewer reads no merchant configuration")
	require.Equal(t, http.StatusOK, call(t, handler, ownerKey.Secret, http.MethodGet, "/v1/admin/psps", "", nil).Code)
	require.Equal(t, http.StatusConflict, findings(ownerKey.Secret, "id:"+other.String()), "a key acts at its own merchant only")
	require.Equal(t, http.StatusOK, findings(merchantKey(t, srv, other, "viewer").Secret, ""))
	check := map[string]any{"customer_id": billing.CustomerID(uuid.New()).String(), "entitlements": []string{"content:any"}}
	w := call(t, handler, viewerKey.Secret, http.MethodPost, "/v1/app/entitlements/check", "", check)
	require.Equal(t, http.StatusForbidden, w.Code, "a viewer key holds no programmatic permission: %s", w.Body.String())
	require.Equal(t, http.StatusOK, call(t, handler, ownerKey.Secret, http.MethodPost, "/v1/app/entitlements/check", "", check).Code, "the owner's merchant:* holds merchant:entitlements:read")
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/merchant/team"},
		{http.MethodPost, "/v1/merchant/api-keys"},
		{http.MethodGet, "/v1/merchants"},
	} {
		w := call(t, handler, ownerKey.Secret, route.method, route.path, "", map[string]any{})
		require.Equal(t, http.StatusNotFound, w.Code, "%s %s is not mounted: %s", route.method, route.path, w.Body.String())
	}
	require.NoError(t, srv.AuthKit().RevokeAPIKey(ctx, iam.SystemIdentity(), iam.GroupByID(mid.String()), viewerKey.APIKey.ID))
	require.Equal(t, http.StatusUnauthorized, findings(viewerKey.Secret, ""), "a revoked key authenticates nothing")
	require.Equal(t, http.StatusOK, findings(ownerKey.Secret, ""))
}

// merchantKey is an API key of merchant mid's AuthKit group holding the
// merchant role role.
func merchantKey(t *testing.T, srv *server.Server, mid billing.MerchantID, role string) iam.APIKeyCreated {
	t.Helper()
	r, ok := server.MerchantRole(role)
	require.True(t, ok, role)
	created, err := srv.AuthKit().CreateAPIKey(t.Context(), iam.SystemIdentity(), iam.GroupByID(mid.String()), iam.NewAPIKey{Name: role, Role: r})
	require.NoError(t, err)
	return created
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

// A person names the merchant they act on (its API host or the
// OpenRails-Merchant selector, as the console does); holding roles in
// several, or one, never picks it for them. Their permission is asked of
// AuthKit in that merchant's group.
func TestStaffNameTheirMerchant(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	ctx := t.Context()
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	member, token := newOwner(t, srv)
	mine, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("mine"), OwnerUserID: member})
	require.NoError(t, err)
	theirs, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("theirs"), OwnerUserID: newAccount(t, srv).ID})
	require.NoError(t, err)

	findings := func(selector string) *httptest.ResponseRecorder {
		return call(t, handler, token, http.MethodGet, "/v1/admin/findings", selector, nil)
	}
	w := findings("")
	require.Equal(t, http.StatusForbidden, w.Code, "no merchant named: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "merchant_unresolved")
	require.Equal(t, http.StatusOK, findings("id:"+mine.MerchantID.String()).Code)
	w = findings("id:" + theirs.MerchantID.String())
	require.Equal(t, http.StatusForbidden, w.Code, "a merchant they hold no role in: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "permission_required")

	listed, err := srv.ListUserMerchants(ctx, member)
	require.NoError(t, err)
	require.Len(t, listed, 1, "a hosted product's list for a user AuthKit authenticated")
	require.Equal(t, mine.MerchantID, listed[0].ID)
	require.Equal(t, "owner", listed[0].Role)
	require.Equal(t, []string{"merchant:*"}, listed[0].Permissions)
}
