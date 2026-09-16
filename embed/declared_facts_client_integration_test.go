//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/embedded"
)

// fakeMintReader serves an initialized SPL mint at a fixed decimals count so
// the Solana projection does not depend on a chain read.
type fakeMintReader struct{ decimals uint8 }

func (r fakeMintReader) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	blob := make([]byte, solanaint.MintAccountSize)
	blob[44] = r.decimals
	blob[45] = 1
	return blob, nil
}

// The platform/host operations that still bypassed the Client (customer
// materialization, declared billing facts with admin comps, Solana checkout
// discovery) run identically through the in-process and HTTP transports.
func TestDeclaredFactsAndCustomerOpsThroughSharedClient(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	const wallet = "9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu"
	rails := config.PSPSet{
		"ccbill": {AccountID: "999982-0000", CCBill: &config.CCBillRailConfig{Salt: "declared-facts-fixture"}},
		"solana": {AccountID: wallet, Solana: &config.SolanaRailConfig{Tokens: map[string]config.TokenConfig{"USDC": {Name: "USD Coin"}}}},
	}
	remote := h.StartStandalone("USD", integrationharness.WithRails(rails))
	remote.App().Runtime.SolanaMintDecimals = solanamodule.NewMintDecimals(fakeMintReader{decimals: 6})
	runtime, err := embed.New(ctx, embed.Options{Options: embedded.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embedded.RiverManagedByOpenRails(),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	runtime.Embedded().App().Runtime.SolanaMintDecimals = solanamodule.NewMintDecimals(fakeMintReader{decimals: 6})
	local, err := runtime.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	mid := dbtest.TestMerchantID
	pool := h.MerchantPool(mid.UUID())

	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": remote.Client()} {
		t.Run(name, func(t *testing.T) {
			checkout, err := client.GetCheckoutConfig(ctx)
			require.NoError(t, err)
			require.NotNil(t, checkout.Solana, "an armed Solana PSP is discoverable from the checkout document")
			require.Equal(t, "devnet", checkout.Solana.Network)
			require.Equal(t, "solana:devnet", checkout.Solana.Chain)
			require.Equal(t, "USDC", checkout.Solana.PreferredToken)
			require.Len(t, checkout.Solana.Tokens, 1)
			require.Equal(t, openrails.SolanaCheckoutToken{Symbol: "USDC", Name: "USD Coin", Mint: checkout.Solana.Tokens[0].Mint, Decimals: 6, Preferred: true, RecurringEligible: true}, checkout.Solana.Tokens[0])
			require.NotEmpty(t, checkout.Solana.Tokens[0].Mint)

			customer := openrails.CustomerID(uuid.New())
			first, err := client.EnsureCustomer(ctx, customer)
			require.NoError(t, err)
			require.Equal(t, customer, first.ID)
			require.False(t, first.CreatedAt.IsZero())
			again, err := client.EnsureCustomer(ctx, customer)
			require.NoError(t, err)
			require.Equal(t, first.ID, again.ID)
			require.True(t, again.CreatedAt.Equal(first.CreatedAt), "a second touch keeps the record")
			require.False(t, again.LastSeenAt.Before(first.LastSeenAt))
			_, err = client.EnsureCustomer(ctx, openrails.CustomerID(uuid.Nil))
			require.ErrorIs(t, err, openrails.ErrInvalid)

			product, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: "facts-" + uuid.NewString(), DisplayName: "Comped", EntitlementsSpec: map[string]*int{"premium": nil}})
			require.NoError(t, err)
			bare, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: "bare-" + uuid.NewString(), DisplayName: "No spec"})
			require.NoError(t, err)
			duration := 720
			price, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: "facts-" + uuid.NewString(), UnitAmount: 0, Currency: "USD", AccessDurationHours: &duration})
			require.NoError(t, err)

			comped, subscriber := uuid.New(), uuid.New()
			now := time.Now().UTC().Truncate(time.Second)
			end := now.Add(48 * time.Hour)
			source := "comp-" + uuid.NewString()
			trial := "trial-" + uuid.NewString()
			book := openrails.DeclaredBilling{
				AsOf:       now,
				DefaultPSP: openrails.PSPRef{Key: "ccbill"},
				Customers:  []openrails.DeclaredCustomer{{Customer: subscriber}},
				Subscriptions: []openrails.DeclaredSubscription{{
					SourceID: trial, Customer: subscriber, Price: price.ID, Rail: "ccbill", RailSubscriptionID: trial,
					StartedAt: now.Add(-time.Hour), PaidThrough: &end,
				}},
				AdminGrants: []openrails.DeclaredAdminGrant{
					{Customer: comped, Product: product.ID, SourceID: source, StartsAt: now.Add(-time.Hour), EndsAt: &end},
					{Customer: comped, Product: bare.ID, SourceID: source + "-nospec", StartsAt: now},
				},
			}
			result, err := client.ImportBilling(ctx, book)
			require.NoError(t, err)
			require.ElementsMatch(t, []string{source, trial}, result.Imported)
			require.Equal(t, []string{source + "-nospec"}, result.Blocked)
			require.Equal(t, "product has no entitlements_spec", result.Reasons[source+"-nospec"])
			has, err := client.HasEntitlement(ctx, comped.String(), "premium", time.Time{})
			require.NoError(t, err)
			require.True(t, has, "the comp materialized its entitlement window")
			subscriptions, err := client.ListSubscriptions(ctx, openrails.SubscriptionFilter{CustomerID: subscriber.String()})
			require.NoError(t, err)
			require.Len(t, subscriptions.Data, 1)
			require.Equal(t, price.ID.String(), subscriptions.Data[0].Price.ID)

			replay, err := client.ImportBilling(ctx, book)
			require.NoError(t, err)
			require.ElementsMatch(t, []string{source, trial}, replay.Skipped, "re-declaring the same facts is a no-op")
			require.Empty(t, replay.Imported)
			_, err = client.ImportBilling(ctx, openrails.DeclaredBilling{AdminGrants: book.AdminGrants})
			require.ErrorIs(t, err, openrails.ErrInvalid, "as_of is required")

			var windows int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND source_type='admin'`, mid.UUID(), comped).Scan(&windows))
			require.Equal(t, 1, windows)
		})
	}

	reader, err := openrails.NewRemote(remote.BaseURL, openrails.WithAPIKey(remote.MintAPIKey(dbtest.TestMerchantSlug, "facts-reader", []string{permissions.MerchantCatalogRead})))
	require.NoError(t, err)
	_, err = reader.EnsureCustomer(ctx, openrails.CustomerID(uuid.New()))
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = reader.ImportBilling(ctx, openrails.DeclaredBilling{AsOf: time.Now()})
	require.ErrorIs(t, err, openrails.ErrDenied)
}
