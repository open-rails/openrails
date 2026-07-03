//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #699: a doujins-shaped embedded boot — NO Options.PaymentProviders, provider
// credentials declared ONLY through the merchant manifest (UpsertMerchantConfig
// seeds the per-merchant secrets store) — arms the pull plane: worker
// registration builds the merchants service, and the per-merchant builder
// yields fetchers + probers for the seeded rails with the seeded credentials.
func TestEmbeddedPullArming_ManifestSecretsNoPaymentProviders(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("embed-pull-arm-%d", nano)
	nmiAccount := fmt.Sprintf("77%d", nano%1_000_000)
	ccbillAccount := fmt.Sprintf("91%04d-0000", nano%10_000)
	securityKey := fmt.Sprintf("sec-key-%d", nano)

	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: dsn}}
	rt, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{Config: cfg}, // deliberately NO PaymentProviders
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	m := embed.MerchantConfig{
		DisplayName: slug,
		RailMerchantAccounts: map[string]embed.RailMerchantAccountConfig{
			"mobius": {
				"nmi": {
					Environment: "live",
					AccountID:   nmiAccount,
					Secrets:     map[string]string{"security_key": securityKey},
				},
			},
			"ccbill": {
				"ccbill": {
					Environment: "live",
					AccountID:   ccbillAccount,
					Secrets: map[string]string{
						"datalink_username": "dl-user-" + slug,
						"datalink_password": "dl-pass-" + slug,
					},
				},
			},
		},
	}
	id, err := rt.UpsertMerchantConfig(ctx, slug, m)
	require.NoError(t, err)
	require.False(t, id.IsZero())
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM openrails.merchant_secrets WHERE merchant_id = $1`,
			`DELETE FROM openrails.rail_merchant_accounts WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = appDB.Pool().Exec(context.Background(), stmt, id.UUID())
		}
	})

	runtime := rt.Embedded().App().Runtime
	require.NotNil(t, runtime)

	// Worker registration is where hosts fold OpenRails' workers into their
	// River client (doujins RegisterRiverWorkers) — it must arm the merchants
	// service even though no standalone HTTP server ever runs.
	require.NoError(t, runtime.AddBillingWorkersTo(ctx, river.NewWorkers()))
	require.NotNil(t, runtime.Merchants, "#699: worker registration builds the merchants service from the store")

	// The per-merchant pull plane the refresh job builds inside the merchant
	// loop: manifest-seeded credentials arm fetchers AND probers (the
	// unknown-cohort resolution inputs) with no boot-config rails.
	armed := reconcile.MerchantFetcherBuilder{
		Config:         runtime.Config,
		Rails:          runtime.Rails,
		Merchants:      runtime.Merchants,
		DB:             runtime.DB,
		NMIClients:     runtime.NMIClients,
		CCBillDataLink: runtime.CCBillDataLink,
		SolanaRPC:      runtime.SolanaRPC,
	}.Build(merchant.WithID(ctx, id), id)

	require.Contains(t, armed.Fetchers, reconcile.ProviderNMI, "NMI fetcher armed from manifest secrets")
	require.Contains(t, armed.Fetchers, reconcile.ProviderCCBill, "CCBill fetcher armed from manifest secrets")

	nmiProber, ok := armed.Probers[reconcile.ProviderNMI].(*reconcile.NMISubscriptionProber)
	require.True(t, ok, "NMI per-subscription prober armed")
	assert.Equal(t, securityKey, nmiProber.Client.SecurityKey, "prober carries the merchant's own key")

	require.Contains(t, armed.Probers, reconcile.ProviderCCBill, "CCBill per-subscription prober armed (#696)")
	require.NotNil(t, armed.CCBillDataLink, "DataLink bulk lane armed")
	assert.Equal(t, "dl-user-"+slug, armed.CCBillDataLink.Username)
	assert.Equal(t, "dl-pass-"+slug, armed.CCBillDataLink.Password)
}
