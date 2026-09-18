//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

func TestMeteringClientRatesIntoInvoice(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD")
	runtime, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": remote.Client()} {
		t.Run(name, func(t *testing.T) {
			product, payer := uuid.New(), uuid.New()
			key := "meter-client-" + uuid.NewString()
			_, err := h.Pool().Exec(ctx, `INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Usage product')`, product, dbtest.TestMerchantID.UUID(), key)
			require.NoError(t, err)
			spec := openrails.UsageMeterSpec{Key: key, EventType: key, Aggregation: "sum", ValueProperty: "units", Unit: "requests"}
			require.NoError(t, client.EnsureUsageMeter(ctx, spec))
			first, err := client.GetUsageMeter(ctx, key)
			require.NoError(t, err)
			require.NoError(t, client.EnsureUsageMeter(ctx, spec))
			again, err := client.GetUsageMeter(ctx, key)
			require.NoError(t, err)
			require.True(t, first.UpdatedAt.Equal(again.UpdatedAt))
			card, err := client.SetDefaultUsageRateCard(ctx, key, openrails.DefaultUsageRateCardRequest{ProductID: openrails.ProductID(product), Price: pricing.RatePrice{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{UnitAmount: 100, DivideBy: 1}}})
			require.NoError(t, err)
			require.NotNil(t, card.DefaultRateCard)
			require.EqualValues(t, 100, card.DefaultRateCard.Price.PerUnit.UnitAmount)
			ms := remote.App().Runtime.MoneyService
			mode := money.BillingModeArrears
			_, err = ms.UpsertAccountSettings(dbtest.WithTestMerchant(ctx), identity.CustomerID(payer), "USD", money.AccountSettingsInput{BillingMode: &mode})
			require.NoError(t, err)
			event := openrails.UsageReport{CustomerID: openrails.CustomerID(payer), Currency: "USD", Invoker: "host", EventType: key, Dimensions: map[string]int64{"units": 3}, Source: "workflow", SourceID: uuid.NewString()}
			require.NoError(t, client.RecordUsage(ctx, event))
			require.NoError(t, client.RecordUsage(ctx, event))
			// Drive the same close used by the invoice worker, then read with the client.
			from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
			invoice, err := ms.FinalizeInvoice(dbtest.WithTestMerchant(ctx), identity.CustomerID(payer), "USD", from, to)
			require.NoError(t, err)
			read, err := client.GetMerchantInvoice(ctx, invoice.ID)
			require.NoError(t, err)
			require.EqualValues(t, 300, read.AmountDue)
			replay, err := ms.FinalizeInvoice(dbtest.WithTestMerchant(ctx), identity.CustomerID(payer), "USD", from, to)
			require.NoError(t, err)
			require.Equal(t, invoice.ID, replay.ID)
			meters, total, err := client.ListUsageMeters(ctx, openrails.PageOptions{Limit: 100})
			require.NoError(t, err)
			require.Positive(t, total)
			require.NotEmpty(t, meters)
		})
	}
}
