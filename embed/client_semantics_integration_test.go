//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Consumer-found Client semantics hold on both transports: an unset archived
// filter lists every price, a same-instant entitlement read sees the grant it
// was made in, and a PSP declared without credentials never counts as armed.
func TestClientSemanticsAcrossTransports(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD")
	// A merchant of its own: the declared PSP must not leak into the shared
	// test merchant's checkout document.
	owned := remote.ProvisionOwnedMerchant("semantics-" + uuid.NewString()[:8])
	mid := owned.MerchantID
	runtime, err := embed.New(ctx, embed.Options{Options: embedded.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embedded.RiverManagedByOpenRails(),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client(openrails.WithMerchantID(mid))
	require.NoError(t, err)
	standalone := remote.Client(openrails.WithTokenProvider(func(context.Context) (string, error) { return owned.APIKey, nil }))

	// A Stripe identity without credentials: attributable, never armed.
	stripePSP, err := runtime.DeclarePSP(ctx, mid, embedded.PSPDeclaration{Key: "stripe-declared", Rail: "stripe", AccountID: "acct_declared_" + uuid.NewString()[:8]})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, stripePSP)
	rails := runtime.Embedded().App().Runtime.RailConfigs
	armed, err := rails.Armed(merchant.WithID(ctx, mid), "stripe")
	require.NoError(t, err)
	require.False(t, armed, "a credential-less PSP is not an armed rail")

	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": standalone} {
		t.Run(name, func(t *testing.T) {
			key := "semantics-" + uuid.NewString()[:8]
			product, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: key, DisplayName: "Semantics"})
			require.NoError(t, err)
			duration := 720
			live, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: key + "-live", UnitAmount: 1_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)
			old, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: key + "-old", UnitAmount: 2_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)
			retired := true
			_, err = client.UpdatePrice(ctx, old.ID, openrails.UpdatePriceRequest{Archived: &retired})
			require.NoError(t, err)

			all, err := client.ListPrices(ctx, openrails.PriceFilter{ProductID: &product.ID})
			require.NoError(t, err)
			require.Len(t, all.Items, 2, "no archived filter lists live and archived prices")
			notArchived := false
			current, err := client.ListPrices(ctx, openrails.PriceFilter{ProductID: &product.ID, Archived: &notArchived})
			require.NoError(t, err)
			require.Len(t, current.Items, 1)
			require.Equal(t, live.ID, current.Items[0].ID)
			archivedOnly, err := client.ListPrices(ctx, openrails.PriceFilter{ProductID: &product.ID, Archived: &retired})
			require.NoError(t, err)
			require.Len(t, archivedOnly.Items, 1)
			require.Equal(t, old.ID, archivedOnly.Items[0].ID)
			products, err := client.ListProducts(ctx, openrails.ProductFilter{PageOptions: openrails.PageOptions{Limit: 1000}})
			require.NoError(t, err)
			require.Positive(t, products.Total)

			// A grant made now is visible at exactly now, sub-second included.
			customer := uuid.NewString()
			granted, err := client.GrantEntitlement(ctx, customer, openrails.GrantEntitlementRequest{Entitlement: "premium"})
			require.NoError(t, err)
			at := granted.StartAt.Add(time.Microsecond)
			require.NotZero(t, at.Nanosecond()%int(time.Second), "the fixture instant must carry sub-second precision")
			records, err := client.ListActiveEntitlements(ctx, []string{customer}, at)
			require.NoError(t, err)
			require.Len(t, records[customer], 1)
			has, err := client.HasEntitlement(ctx, customer, "premium", at)
			require.NoError(t, err)
			require.True(t, has)
			holders, err := client.ListCustomersWithEntitlement(ctx, "premium", at)
			require.NoError(t, err)
			require.Contains(t, holders, customer)
			before, err := client.ListActiveEntitlements(ctx, []string{customer}, granted.StartAt.Add(-time.Microsecond))
			require.NoError(t, err)
			require.Empty(t, before[customer], "one microsecond before the grant it is not active")

			// Linking a price to the declared, unarmed Stripe account stores the
			// link without a provider round trip and reports sync as disabled.
			linked, err := client.UpdatePrice(ctx, live.ID, openrails.UpdatePriceRequest{PSPLinks: map[string]map[string]string{"stripe-declared": {"price_id": "price_declared_" + key}}})
			require.NoError(t, err)
			require.Equal(t, openrails.ProviderStatusLinked, linked.Providers["stripe-declared"].Status)
			require.Equal(t, "price_declared_"+key, linked.Providers["stripe-declared"].IDs["price_id"])
		})
	}
}
