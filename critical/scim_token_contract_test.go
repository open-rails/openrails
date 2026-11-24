//go:build integration

package critical_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// configurationRefusal is what the engine's configuration probe answers.
func configurationRefusal(engine *openrails.Engine) string {
	for _, p := range engine.Probes() {
		if p.Name == "openrails_configuration" {
			if err := p.Check(context.Background()); err != nil {
				return err.Error()
			}
		}
	}
	return ""
}

// A provisioning token resolves to one merchant, whichever presents it, so
// two merchants can never share one: the second declaration is refused, and
// the directory's writes reach only the merchant that holds it.
func TestProvisioningTokenDeclaration(t *testing.T) {
	t.Run("OR-J07-S06/a-shared-scim-token-provisions-only-its-holder", func(t *testing.T) {
		token := "scim-" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
		a := newAuthorityWorld(t, func(w *world) {
			w.deps.UserInfo = nil
			for _, slug := range []string{"alpha", "beta"} {
				declaration := cfgMerchant(w.cfg, slug)
				declaration.Secrets.SCIMToken = token
				setCfgMerchant(&w.cfg, slug, declaration)
			}
		})
		// Both merchants load their configuration; one holds the token.
		_, _ = a.alpha.GetMerchantConfiguration(t.Context())
		_, _ = a.beta.GetMerchantConfiguration(t.Context())
		var refused string
		require.Eventually(t, func() bool {
			refused = configurationRefusal(a.engine)
			return strings.Contains(refused, "secrets.scim_token")
		}, 10*time.Second, 50*time.Millisecond, "the second merchant declaring the token is refused: %q", refused)

		id := uuid.NewString()
		input := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": "shared-token", "displayName": "Shared Token"}
		status, raw := a.request(t, "POST", "/v1/app/scim/v2/Users", token, input)
		require.Equal(t, 201, status, string(raw))
		customer := billing.CustomerID(uuid.MustParse(id))
		_, alphaErr := a.alpha.GetCustomer(t.Context(), customer)
		_, betaErr := a.beta.GetCustomer(t.Context(), customer)
		require.True(t, (alphaErr == nil) != (betaErr == nil), "exactly one merchant has the provisioned customer: alpha %v, beta %v", alphaErr, betaErr)
		holder, refusedSlug := "alpha", "beta"
		if betaErr == nil {
			holder, refusedSlug = "beta", "alpha"
		}
		other := func() *openrails.Client {
			if refusedSlug == "alpha" {
				return a.alpha
			}
			return a.beta
		}
		require.NotContains(t, refused, "merchants."+holder+".", "the holder serves its configuration")
		require.Contains(t, refused, "merchants."+refusedSlug+".merchant: secrets.scim_token")
		_, err := other().GetCustomer(t.Context(), customer)
		require.ErrorIs(t, err, billing.ErrNotFound)

		// Declaring it again for its holder is a no-op; the other stays refused.
		a.restart(t)
		_, _ = a.alpha.GetMerchantConfiguration(t.Context())
		_, _ = a.beta.GetMerchantConfiguration(t.Context())
		require.Eventually(t, func() bool {
			refused = configurationRefusal(a.engine)
			return strings.Contains(refused, "secrets.scim_token")
		}, 10*time.Second, 50*time.Millisecond)
		require.NotContains(t, refused, "merchants."+holder+".")
		second := uuid.NewString()
		status, raw = a.request(t, "POST", "/v1/app/scim/v2/Users", token, map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": second, "userName": "after-restart"})
		require.Equal(t, 201, status, string(raw))
		_, err = other().GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(second)))
		require.ErrorIs(t, err, billing.ErrNotFound, "a restart never moves the token to the other merchant")
	})
}
