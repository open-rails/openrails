//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/stretchr/testify/require"
)

// A host builds the openrails-checkout session document from shared Client
// reads. The document must be byte-identical whether those reads are embedded
// or standalone HTTP, and its money must survive the remote hop above 2^53.
func TestHostedCheckoutDocumentParity(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	// The same CCBill account the package's other tests arm: the shared test
	// merchant must stay single-account on the rail.
	remote := h.StartStandalone("USD", integrationharness.WithRails(config.PSPSet{
		"ccbill": {AccountID: "999981-0000", CCBill: &config.CCBillRailConfig{Salt: "issue983-local-fixture"}},
	}))
	local, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close(context.Background())) })
	app.HostGraph(local).Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)

	const unitAmount = int64(9007199254740993) // 2^53 + 1: exact only as a decimal string
	productID, priceID := uuid.New(), uuid.New()
	priceKey := "hosted-" + priceID.String()
	_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Hosted fixture')`, productID, dbtest.TestMerchantID.UUID(), "hosted-"+productID.String())
	require.NoError(t, err)
	_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,$5,'USD',720,true)`, priceID, dbtest.TestMerchantID.UUID(), productID, priceKey, unitAmount)
	require.NoError(t, err)
	pspID := dbtest.EnsureTestPSP(ctx, t, h.Pool(), dbtest.TestMerchantID.UUID(), "ccbill")
	_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,flex_id,configuration) VALUES($1,$2,$3,$4,'{"form_name":"test-form"}')`, dbtest.TestMerchantID.UUID(), priceID, pspID, uuid.NewString())
	require.NoError(t, err)

	inprocess, err := local.Client()
	require.NoError(t, err)
	expires := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	documents := map[string][]byte{}
	for name, client := range map[string]*openrails.Client{"embedded": inprocess, "remote": remote.Client()} {
		t.Run(name, func(t *testing.T) {
			price, err := client.GetPriceByKey(ctx, priceKey)
			require.NoError(t, err)
			require.Equal(t, unitAmount, price.UnitAmount)
			product, err := client.GetProduct(ctx, price.ProductID)
			require.NoError(t, err)
			plan, err := openrails.NewHostedCheckoutPlan(product, price)
			require.NoError(t, err)
			options, err := client.ListCheckoutRailOptions(ctx, priceKey)
			require.NoError(t, err)
			require.NotEmpty(t, options)
			document := openrails.HostedCheckoutSession{ID: "ocs_parity", Status: "created", Merchant: openrails.HostedCheckoutMerchant{DisplayName: "Parity"}, Plan: plan, ExpiresAt: expires}
			for _, option := range options {
				driver, ok := openrails.HostedCheckoutDriver(option.Rail)
				require.True(t, ok, option.Rail)
				document.Rails = append(document.Rails, openrails.HostedCheckoutRail{ID: "option_" + option.PSPID, Rail: option.Rail, Mode: option.Mode, Driver: driver})
			}
			raw, err := json.Marshal(document)
			require.NoError(t, err)
			documents[name] = raw
			var wire map[string]any
			require.NoError(t, json.Unmarshal(raw, &wire))
			plan_ := wire["plan"].(map[string]any)
			require.Equal(t, "9007199254740993", plan_["unit_amount"])
			require.Equal(t, "USD", plan_["currency"])
			require.Equal(t, float64(6), plan_["unit_decimals"])
			require.Equal(t, float64(720), plan_["period_hours"])
			require.Equal(t, true, plan_["automatically_renews"])
			require.Equal(t, "Hosted fixture", plan_["display_name"])
		})
	}
	require.JSONEq(t, string(documents["embedded"]), string(documents["remote"]), "embedded and standalone hosts must serve the same hosted checkout document")
}
