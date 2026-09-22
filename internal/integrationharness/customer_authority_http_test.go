//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/verify"
	orauthkit "github.com/open-rails/openrails/embed/authkit"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
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
	host, err = orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), dbtest.TestMerchantID.String(), orauthkit.WithPermissionResolver(func(context.Context, *http.Request, verify.Claims) ([]string, error) {
		return []string{permissions.CustomerAll}, nil
	}))
	require.NoError(t, err)
	path := surface.BaseURL + "/v1/customers/" + user.ID + "/spend-delegations"
	delegate := "worker-" + uuid.NewString()
	window := map[string]any{"key": "day", "window_seconds": 86400, "limit": 10, "currency": "USD"}
	document := map[string]any{"delegations": []map[string]any{{"scope": "invoker", "scope_key": delegate, "windows": []map[string]any{window}}}}
	status, raw := requestJSON(t, http.MethodPut, path, token, document)
	require.Equal(t, 400, status, "numeric money must not be accepted: %s", raw)
	window["limit"] = "10"
	status, raw = requestJSON(t, http.MethodPut, path, token, document)
	require.Equal(t, 200, status, string(raw))
	status, raw = requestJSON(t, http.MethodGet, path, token, nil)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), delegate)
	require.Contains(t, string(raw), `"limit":"10"`)
	status, raw = requestJSON(t, http.MethodDelete, path+"/invoker/"+delegate, token, nil)
	require.Equal(t, 200, status, string(raw))
	_, err = core.ResolveGroupIDForSlug(ctx, controlplane.CustomerGroup(user.ID))
	require.ErrorIs(t, err, authkit.ErrGroupNotFound)
}
