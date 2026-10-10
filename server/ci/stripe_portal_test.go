//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/open-rails/openrails/internal/vaultfake"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/operator"
)

// On a server of many merchants, Stripe's customer portal is a Stripe
// merchant's route: its customer gets a session for their Stripe customer,
// returning to an allowed origin. A merchant without a Stripe PSP answers
// as though the route were not mounted.
func TestStripePortalIsAStripeMerchantsRoute(t *testing.T) {
	f := newFixture(t)
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	vault := vaultfake.New("e2e-root")
	t.Cleanup(vault.Close)
	host := newIssuerKey(t, "https://portal-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	plainHost := newIssuerKey(t, "https://portal-plain-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	shop, plain := uniqueName("portal-stripe"), uniqueName("portal-plain")
	cp := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.Vault = &openrails.VaultConfig{Address: vault.URL(), Token: vault.Token}
		deps.Engine.StripeTransport = stripe
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
	})
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(fmt.Sprintf(`version: 1
merchants:
  %s:
    display_name: Stripe Shop
    psps:
      stripe:
        rail: stripe
        account_id: acct_portal
        secrets: {secret_key: sk_test_portal, webhook_signing_secret: whsec_portal}
        settings: {publishable_key: pk_test_portal}
  %s:
    display_name: Plain Shop
`, shop, plain)), 0o600))
	graph, plane := operator.Of(cp)
	require.NoError(t, serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, plane, manifest, nil, ""))
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)

	m, err := graph.Runtime.Merchants.GetBySlug(t.Context(), shop)
	require.NoError(t, err)
	p, err := graph.Runtime.Merchants.GetBySlug(t.Context(), plain)
	require.NoError(t, err)
	trust(t, cp, m.ID, host.app(t, "viewer", nil))
	trust(t, cp, p.ID, plainHost.app(t, "viewer", nil))

	customerToken := func(k issuerKey) string {
		return k.mint(t, func(c jwt.MapClaims) {
			c["sub"], c["scope"] = uuid.NewString(), billing.ScopeSelf
			delete(c, "permissions")
		})
	}
	tokens := map[string]string{shop: customerToken(host), plain: customerToken(plainHost)}
	me := func(merchant, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		return serve(handler, rsRequest{method: http.MethodPost, path: "/v1/me" + path, authorization: "Bearer " + tokens[merchant], selector: merchant, body: body, idempotencyKey: uuid.NewString()})
	}
	const portal = "/stripe/billing-portal-sessions"

	w := me(shop, portal, "")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.Equal(t, billing.CodeResourceNotFound, errorCode(t, w), "served, but the customer has no Stripe customer yet")

	psps, err := cp.Client().ListPSPs(t.Context(), billing.PSPListParams{}, openrails.ForMerchantID(m.ID))
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	w = me(shop, "/payment-method-setups", fmt.Sprintf(`{"psp_id":%q,"consent":true}`, psps.Items[0].ID))
	require.Equal(t, http.StatusOK, w.Code, "starting a card setup makes the Stripe customer: %s", w.Body.String())

	w = me(shop, portal, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var session struct{ URL string }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &session))
	require.True(t, strings.HasPrefix(session.URL, "https://billing.stripe.com/p/session/"), session.URL)
	sent := stripe.Submitted("/v1/billing_portal/sessions")
	require.Len(t, sent, 1)
	require.Regexp(t, `^cus_`, sent[0].Form.Get("customer"))
	require.Equal(t, "https://e2e.test/account", sent[0].Form.Get("return_url"))

	for _, tc := range []struct{ merchant, path string }{
		{plain, portal},
		{shop, "/billing-portal-sessions"},
	} {
		w := me(tc.merchant, tc.path, "")
		require.Equal(t, http.StatusNotFound, w.Code, "%s %s: %s", tc.merchant, tc.path, w.Body.String())
		require.Equal(t, billing.CodeRouteNotFound, errorCode(t, w), "%s %s", tc.merchant, tc.path)
	}
	require.Len(t, stripe.Submitted("/v1/billing_portal/sessions"), 1)
	require.Empty(t, stripe.Unexpected())
}
