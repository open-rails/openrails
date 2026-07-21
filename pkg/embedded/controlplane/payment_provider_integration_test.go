//go:build integration

package controlplane_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func TestUpsertPaymentProviderConfig(t *testing.T) {
	ctx := context.Background()
	cfg := hostedTestConfig(dbtest.SharedPostgresDSN(t), "https://providers.openrails.test")
	e := newHostApp(t, cfg)
	require.NoError(t, embcp.Attach(ctx, e.App(), cfg, nil))
	require.NoError(t, e.App().Runtime.EnsureMerchantsService(ctx))

	suffix := strings.ToLower(uuid.NewString()[:8])
	provisioned, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: "provider-" + suffix})
	require.NoError(t, err)

	const signingSecret = "whsec_integration_test"
	provider, err := embcp.UpsertPaymentProviderConfig(ctx, e.App(), provisioned.MerchantID, "stripe", embcp.UpsertPaymentProviderConfigRequest{
		Environment: "test",
		AccountID:   "acct_" + suffix,
		PublicConfig: map[string]string{
			"publishable_key": "pk_test_" + suffix,
		},
		Credentials: map[string]string{
			"webhook_signing_secret": signingSecret,
		},
	})
	require.NoError(t, err)
	require.Equal(t, "stripe", provider.Rail)
	require.Equal(t, "test", provider.Environment)
	require.Equal(t, "acct_"+suffix, provider.AccountID)
	require.True(t, provider.Credentials["webhook_signing_secret"].Configured)

	encoded, err := json.Marshal(provider)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), signingSecret)
}
