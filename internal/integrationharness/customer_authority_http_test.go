//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	orauthkit "github.com/open-rails/openrails/pkg/embedded/authkit"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/stretchr/testify/require"
)

// A real host AuthKit principal may manage its merchant-scoped billing policy
// without implicitly creating a separate SaaS portal membership or credentials.
func TestCustomerTreasuryWritesDoNotCreatePortalAuthority(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	var host billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("usd", func(cfg *standaloneConfig) {
		cfg.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			return host.AuthenticateDelegated(ctx, r)
		})
	})
	cp := embcp.Get(surface.App())
	core := cp.Core()
	user, err := core.CreateUser(ctx, "customer-authority-"+uuid.NewString()+"@example.test", "customer"+uuid.NewString()[:8])
	require.NoError(t, err)
	token, _, err := core.MintAccessToken(ctx, user.ID, nil)
	require.NoError(t, err)
	_, err = cp.TouchCustomer(ctx, dbtest.TestMerchantID, "https://controlplane.openrails.test", user.ID)
	require.NoError(t, err)
	host, err = orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), dbtest.TestMerchantID.String(), orauthkit.WithRolePermissions(func([]string) []string { return []string{permissions.CustomerAll} }))
	require.NoError(t, err)
	path := surface.BaseURL + "/v1/customers/" + user.ID + "/spend-delegations"
	delegate := "worker-" + uuid.NewString()
	document := map[string]any{"delegations": []map[string]any{{"scope": "invoker", "scope_key": delegate, "windows": []map[string]any{{"key": "day", "window_seconds": 86400, "limit": "10", "currency": "USD"}}}}}
	status, raw := requestJSON(t, http.MethodPut, path, token, document)
	require.Equal(t, 200, status, string(raw))
	status, raw = requestJSON(t, http.MethodGet, path, token, nil)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), delegate)
	status, raw = requestJSON(t, http.MethodDelete, path+"/invoker/"+delegate, token, nil)
	require.Equal(t, 200, status, string(raw))
	_, err = core.ResolveGroupIDForSlug(ctx, controlplane.CustomerGroup(user.ID))
	require.ErrorIs(t, err, authkit.ErrGroupNotFound)
}
