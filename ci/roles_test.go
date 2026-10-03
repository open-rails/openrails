//go:build e2e && integration

package ci_test

import (
	"context"
	"strings"
	"testing"

	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/controlplane"
	embcp "github.com/open-rails/openrails/internal/embedcontrolplane"
	"github.com/open-rails/openrails/internal/hostconfig"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/standalonedb"
	"github.com/open-rails/openrails/permissions"
)

// The merchant roles' authority is AuthKit's running catalog, not a copy:
// support, viewer and creator hold no owner authority, and a non-user
// credential hands out only a role its grants cover.
func TestMerchantRolePermissionsInTheRunningCatalog(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, standalonedb.ApplyAuthKit(t.Context(), f.pool))
	rt, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{
			TestMode:          config.CredentialPostureSandbox,
			ProviderWriteMode: config.ProviderWriteModeReadOnly,
			DB:                &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
			ReturnOrigins:     []string{"https://e2e.test"},
		},
		PGXPool: f.pool,
		River:   embed.RiverManagedByOpenRails(f.schema),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	_, err = embcp.Attach(t.Context(), rt, embcp.Options{Auth: &hostconfig.AuthConfig{
		Issuer: "http://127.0.0.1/" + f.schema, AllowMemory: true, AllowMissingSenders: true,
		AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, DirectPeerIP: true, KeysPath: t.TempDir(),
	}})
	require.NoError(t, err)
	cp := operator.Get(app.HostGraph(rt))
	require.NotNil(t, cp)

	held := func(role iam.Role) []string {
		t.Helper()
		perms, err := cp.Core().RolePermissions(role)
		require.NoError(t, err)
		out := make([]string, len(perms))
		for i, p := range perms {
			out[i] = p.String()
		}
		return out
	}
	require.Equal(t, []string{permissions.MerchantAll}, held(controlplane.MerchantOwner))
	require.ElementsMatch(t, []string{permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate}, held(controlplane.MerchantCreator))
	for _, p := range held(controlplane.MerchantViewer) {
		require.True(t, strings.HasSuffix(p, ":read"), "viewer is read-only: %s", p)
	}
	ownerOnly := []string{
		permissions.MerchantSettingsUpdate, permissions.MerchantPaymentProvidersUpdate, permissions.MerchantCatalogUpdate,
		permissions.MerchantCreditsGrant, permissions.MerchantCreditsRevoke, permissions.MerchantCredentialsManage,
		permissions.MerchantMembersRead, permissions.MerchantMembersManage, permissions.MerchantBillingImport,
		permissions.MerchantBillingExport, permissions.MerchantAdmissionsCreate, permissions.MerchantCheckoutCreate,
	}
	for _, role := range []iam.Role{controlplane.MerchantCreator, controlplane.MerchantSupport, controlplane.MerchantViewer} {
		grants := held(role)
		for _, p := range ownerOnly {
			require.False(t, (&controlplane.ResolvedServiceCredential{Permissions: grants}).HasPermission(p), "%s must not hold %s", role, p)
		}
	}

	support, viewer := held(controlplane.MerchantSupport), held(controlplane.MerchantViewer)
	for _, tc := range []struct {
		role   iam.Role
		grants []string
		want   bool
	}{
		{controlplane.MerchantOwner, []string{"merchant:*"}, true},
		{controlplane.MerchantSupport, []string{"merchant:*"}, true},
		{controlplane.MerchantViewer, []string{"merchant:*:read"}, true},
		{controlplane.MerchantSupport, []string{"merchant:*:read"}, false},
		{controlplane.MerchantOwner, support, false},
		{controlplane.MerchantViewer, support, false},
		{controlplane.MerchantSupport, viewer, false},
		{controlplane.MerchantViewer, viewer, true},
		{controlplane.MerchantOwner, []string{"customer:*", "root:*"}, false},
		{controlplane.MerchantViewer, nil, false},
	} {
		covered, err := cp.RoleCoveredBy(tc.role, tc.grants)
		require.NoError(t, err)
		require.Equal(t, tc.want, covered, "%s by %v", tc.role, tc.grants)
	}
}
