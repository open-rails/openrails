//go:build integration

package embed_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Operations Doujins, Cozy-art and OpenRails-SaaS reached through the service
// facade run through the shared client on both transports. No provider is
// called: CCBill only needs a local form and the migrated subscription is on a
// rail that cannot be changed server-side.
func TestMerchantOperationsThroughSharedClient(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD", integrationharness.WithRails(config.PSPSet{
		"ccbill": {AccountID: "999981-0000", CCBill: &config.CCBillRailConfig{Salt: "operations-local-fixture"}},
	}))
	runtime, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	mid := dbtest.TestMerchantID

	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": remote.Client()} {
		t.Run(name, func(t *testing.T) {
			checkout, err := client.GetCheckoutConfig(ctx)
			require.NoError(t, err)
			require.Equal(t, "checkout_config", checkout.Object)
			require.Len(t, checkout.PSPs, 1)
			require.Equal(t, "ccbill", checkout.PSPs[0].Key)
			require.Equal(t, "redirect", checkout.PSPs[0].Flow)
			require.Empty(t, checkout.PSPs[0].Config, "a redirect rail exposes no browser values")

			customer := uuid.NewString()
			staff, err := client.GrantEntitlement(ctx, customer, openrails.GrantEntitlementRequest{Entitlement: "premium"})
			require.NoError(t, err)
			require.Equal(t, "admin", staff.SourceType)
			require.Nil(t, staff.EndAt)
			end := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
			fixed, err := client.GrantEntitlement(ctx, customer, openrails.GrantEntitlementRequest{Entitlement: "beta", EndAt: &end})
			require.NoError(t, err)
			require.True(t, fixed.EndAt.Equal(end))
			hours := 1
			_, err = client.GrantEntitlement(ctx, customer, openrails.GrantEntitlementRequest{Entitlement: "beta", EndAt: &end, Hours: &hours})
			require.ErrorIs(t, err, openrails.ErrInvalid)
			has, err := client.HasEntitlement(ctx, customer, "premium", time.Time{})
			require.NoError(t, err)
			require.True(t, has)
			require.ErrorIs(t, client.RevokeEntitlement(ctx, uuid.NewString(), staff.ID), openrails.ErrNotFound, "another customer's window is not addressable")
			require.NoError(t, client.RevokeEntitlement(ctx, customer, staff.ID))
			has, err = client.HasEntitlement(ctx, customer, "premium", time.Time{})
			require.NoError(t, err)
			require.False(t, has)

			key := "operations-" + uuid.NewString()
			source, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: key + "-a", DisplayName: "Plan A"})
			require.NoError(t, err)
			target, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: key + "-b", DisplayName: "Plan B"})
			require.NoError(t, err)
			duration := 720
			sourcePrice, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: source.ID, Key: key + "-a-monthly", UnitAmount: 1_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)
			targetPrice, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: target.ID, Key: key + "-b-monthly", UnitAmount: 1_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)
			renamed, err := client.SetPriceKey(ctx, sourcePrice.ID, key+"-legacy")
			require.NoError(t, err)
			require.Equal(t, sourcePrice.ID, renamed.ID)
			byKey, err := client.GetPriceByKey(ctx, key+"-legacy")
			require.NoError(t, err)
			require.Equal(t, sourcePrice.ID, byKey.ID)

			subscriber, subscription := uuid.New(), uuid.New()
			psp := dbtest.EnsureTestPSP(ctx, t, h.Pool(), mid.UUID(), "ccbill")
			exec := func(sql string, args ...any) { _, err := h.Pool().Exec(ctx, sql, args...); require.NoError(t, err) }
			exec(`INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), subscriber)
			now := time.Now().UTC()
			exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,'ccbill','active',$7,$8,$9)`,
				subscription, mid.UUID(), subscriber, source.ID, sourcePrice.ID, psp, subscription.String(), now, now.Add(720*time.Hour))
			migration := openrails.PlanMigrationRequest{SourcePrice: key + "-legacy", TargetPrice: targetPrice.ID.String(), FallbackPolicy: "keep_grandfathered"}
			preview, err := client.PreviewPlanMigration(ctx, migration)
			require.NoError(t, err)
			require.Nil(t, preview.BatchID)
			require.Equal(t, 1, preview.Matched)
			require.Equal(t, []openrails.PlanMigrationOutcome{{SubscriptionID: subscription, Rail: "ccbill", Disposition: preview.Outcomes[0].Disposition, Reason: preview.Outcomes[0].Reason}}, preview.Outcomes)
			require.NotEmpty(t, preview.Outcomes[0].Reason)
			created, err := client.CreatePlanMigration(ctx, migration)
			require.NoError(t, err)
			require.NotNil(t, created.BatchID)
			require.Equal(t, preview.Outcomes[0].Disposition, created.Outcomes[0].Disposition)
			canceled, err := client.CancelPlanMigration(ctx, *created.BatchID)
			require.NoError(t, err)
			require.Empty(t, canceled.RailReleaseRequired)
			_, err = client.PreviewPlanMigration(ctx, openrails.PlanMigrationRequest{SourcePrice: sourcePrice.ID.String(), TargetPrice: uuid.NewString()})
			require.ErrorIs(t, err, openrails.ErrNotFound)

			payer := identity.CustomerID(uuid.New())
			rt := remote.App().Runtime
			scoped := merchant.WithID(ctx, mid)
			mode := money.BillingModeArrears
			var invoiceID uuid.UUID
			require.NoError(t, rt.DB.RunInMerchantConn(scoped, func(c context.Context) error {
				if _, err := rt.MoneyService.UpsertAccountSettings(c, payer, "USD", money.AccountSettingsInput{BillingMode: &mode}); err != nil {
					return err
				}
				if _, err := rt.MoneyService.AccrueOwed(c, payer, "USD", "operations", uuid.NewString(), 500); err != nil {
					return err
				}
				invoice, err := rt.MoneyService.FinalizeInvoice(c, payer, "USD", time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
				invoiceID = invoice.ID
				return err
			}))
			payment := openrails.RecordInvoicePaymentRequest{Amount: 100, Reference: "wire-" + uuid.NewString()}
			_, err = client.RecordInvoicePayment(ctx, invoiceID, payment)
			require.NoError(t, err)
			_, err = client.RecordInvoicePayment(ctx, invoiceID, payment)
			requireCode(t, err, openrails.CodeInvoicePaymentReferenceUsed)
			_, err = client.RecordInvoicePayment(ctx, invoiceID, openrails.RecordInvoicePaymentRequest{Amount: 10_000, Reference: "wire-" + uuid.NewString()})
			requireCode(t, err, openrails.CodeInvoicePaymentExceedsDue)
			_, err = client.RecordInvoicePayment(ctx, invoiceID, openrails.RecordInvoicePaymentRequest{Amount: 1})
			require.ErrorIs(t, err, openrails.ErrInvalid)
		})
	}

	reader, err := openrails.NewRemote(remote.BaseURL, openrails.WithAPIKey(remote.MintAPIKey(dbtest.TestMerchantSlug, "operations-reader", []string{permissions.MerchantCatalogRead})))
	require.NoError(t, err)
	_, err = reader.GrantEntitlement(ctx, uuid.NewString(), openrails.GrantEntitlementRequest{Entitlement: "premium"})
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = reader.CreatePlanMigration(ctx, openrails.PlanMigrationRequest{SourcePrice: uuid.NewString(), TargetPrice: uuid.NewString()})
	require.ErrorIs(t, err, openrails.ErrDenied)
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status), "want StatusError, got %v", err)
	require.Equal(t, code, status.Code)
}
