//go:build greenfield && integration

// Package greenfield contains the first replacement CI slice. It deliberately
// talks only to the public embedded API: the old integration harness and
// application tables are not part of this fixture.
package greenfield_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
)

type fixture struct {
	pool   *pgxpool.Pool
	schema string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
	}
	if dsn == "" {
		t.Fatal("OPENRAILS_GREENFIELD_DSN must point at a disposable PostgreSQL database")
	}

	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(t.Context()))

	f := &fixture{pool: pool, schema: "greenfield_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = f.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
		f.pool.Close()
	})

	// The fixture owns no migration files. OpenRails applies its own public
	// migrations, and the second call proves replay is safe before a runtime is
	// constructed.
	for range 2 {
		require.NoError(t, embed.ApplyMigrations(t.Context(), pool, embed.MigrationOptions{
			Schema:      f.schema,
			River:       embed.RiverManagedByOpenRails(f.schema),
			RuntimePool: pool,
		}))
	}
	return f
}

func (f *fixture) runtime(t *testing.T, slug string) (*embed.Runtime, *openrails.Client) {
	return f.runtimeWithStripe(t, slug, nil)
}

func (f *fixture) runtimeWithStripe(t *testing.T, slug string, transport http.RoundTripper) (*embed.Runtime, *openrails.Client) {
	t.Helper()
	providerWriteMode := config.ProviderWriteModeReadOnly
	var stripeTransport http.RoundTripper
	var psps map[string]embed.PSPConfig
	if transport != nil {
		providerWriteMode = config.ProviderWriteModeFull
		stripeTransport = transport
		psps = map[string]embed.PSPConfig{"stripe": {"stripe": {
			AccountID: "acct_greenfield",
			Secrets: map[string]string{
				"secret_key":             "sk_test_greenfield",
				"webhook_signing_secret": "whsec_greenfield",
			},
		}}}
	}
	runtime, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{
			TestMode:            config.CredentialPostureSandbox,
			AllowCatalogUpdates: true,
			ProviderWriteMode:   providerWriteMode,
			DB:                  &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
		},
		Merchant: &embed.MerchantDeclaration{
			Slug:   slug,
			Config: embed.MerchantConfig{DisplayName: slug, PSPs: psps},
		},
		PGXPool:         f.pool,
		River:           embed.RiverManagedByOpenRails(f.schema),
		StripeTransport: stripeTransport,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	client, err := runtime.Client()
	require.NoError(t, err)
	return runtime, client
}

func (f *fixture) dsn(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
	}
	return dsn
}

func TestFreshBootstrapAndReplay(t *testing.T) {
	f := newFixture(t)
	_, client := f.runtime(t, "bootstrap-"+uuid.NewString()[:8])

	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{
		Key:              "welcome-" + uuid.NewString()[:8],
		DisplayName:      "Welcome",
		EntitlementsSpec: map[string]*int{"content:welcome": nil},
	})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{
		ProductID:  product.ID,
		Key:        "welcome-usd-" + uuid.NewString()[:8],
		UnitAmount: 9_007_199_254_740_993,
		Currency:   "USD",
	})
	require.NoError(t, err)
	require.Equal(t, product.ID, price.ProductID)
	require.EqualValues(t, 9_007_199_254_740_993, price.UnitAmount)
	stored, err := client.Prices.Retrieve(t.Context(), price.ID)
	require.NoError(t, err)
	require.Equal(t, price.UnitAmount, stored.UnitAmount, "PostgreSQL and the embedded API preserve amounts above 2^53")

	offers, err := client.ListOffersForEntitlement(t.Context(), "content:welcome", openrails.OfferListParams{Kind: openrails.OfferPermanent})
	require.NoError(t, err)
	require.Len(t, offers.Data, 1)
	require.Equal(t, price.ID, offers.Data[0].PriceID)
}

func TestMerchantCatalogAndCustomerIsolation(t *testing.T) {
	f := newFixture(t)
	_, alice := f.runtime(t, "merchant-a-"+uuid.NewString()[:8])
	_, bob := f.runtime(t, "merchant-b-"+uuid.NewString()[:8])

	productA, err := alice.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "alice-post", DisplayName: "Alice post"})
	require.NoError(t, err)
	productB, err := bob.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "bob-post", DisplayName: "Bob post"})
	require.NoError(t, err)

	_, err = alice.Products.Retrieve(t.Context(), productB.ID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	page, err := alice.Products.List(t.Context(), &openrails.ProductListParams{})
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Total)
	require.Equal(t, productA.ID, page.Items[0].ID)

	customerA := uuid.NewString()
	customerB := uuid.NewString()
	_, err = alice.EnsureCustomer(t.Context(), customerA)
	require.NoError(t, err)
	_, err = bob.EnsureCustomer(t.Context(), customerB)
	require.NoError(t, err)
	_, err = alice.GrantEntitlement(t.Context(), customerA, openrails.GrantEntitlementRequest{Entitlement: "content:" + productA.Key})
	require.NoError(t, err)

	owned, err := alice.CheckEntitlements(t.Context(), customerA, []string{"content:" + productA.Key}, time.Time{})
	require.NoError(t, err)
	require.True(t, owned["content:"+productA.Key])
	foreign, err := bob.CheckEntitlements(t.Context(), customerA, []string{"content:" + productA.Key}, time.Time{})
	require.NoError(t, err)
	require.False(t, foreign["content:"+productA.Key])
}

func TestCatalogEnsureIsIdempotent(t *testing.T) {
	f := newFixture(t)
	_, client := f.runtime(t, "idempotent-"+uuid.NewString()[:8])
	key := "stable-product-" + uuid.NewString()[:8]

	first, err := client.Products.Ensure(t.Context(), &openrails.ProductCreateParams{Key: key, DisplayName: "First title"})
	require.NoError(t, err)
	second, err := client.Products.Ensure(t.Context(), &openrails.ProductCreateParams{Key: key, DisplayName: "Changed title"})
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.DisplayName, second.DisplayName)

	read, err := client.Products.RetrieveByKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, first.ID, read.ID)
	require.Equal(t, first.DisplayName, read.DisplayName)
}

func TestCheckoutReplayAndEntitlementAccess(t *testing.T) {
	f := newFixture(t)
	provider := &stripeCheckoutFake{t: t}
	_, client := f.runtimeWithStripe(t, "checkout-"+uuid.NewString()[:8], provider)

	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{
		Key:              "premium-post-" + uuid.NewString()[:8],
		DisplayName:      "Premium post",
		EntitlementsSpec: map[string]*int{"content:premium": nil},
	})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{
		ProductID:  product.ID,
		Key:        product.Key + "-usd",
		UnitAmount: 1_000_000,
		Currency:   "USD",
	})
	require.NoError(t, err)

	customer := uuid.NewString()
	request := openrails.CreateCheckoutSessionRequest{
		Customer:       openrails.CheckoutCustomerIdentity{ID: customer, VerifiedEmail: "reader@example.test"},
		PriceKey:       price.Key,
		Entitlement:    "content:premium",
		OfferKind:      openrails.OfferPermanent,
		PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "stripe"},
		IdempotencyKey: "checkout-" + uuid.NewString(),
		SuccessURL:     "https://greenfield.test/success",
		CancelURL:      "https://greenfield.test/cancel",
	}
	first, err := client.CreateCheckoutSession(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "stripe", first.RailData["rail"])
	require.NotNil(t, first.PriceID)
	require.Equal(t, price.ID, *first.PriceID)

	replay, err := client.CreateCheckoutSession(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first.ID, replay.ID)
	require.NotNil(t, first.Amount)
	require.EqualValues(t, 1_000_000, *first.Amount)
	require.Equal(t, first.Amount, replay.Amount)
	require.EqualValues(t, 1, provider.checkoutCalls.Load(), "the provider sees one request across an identical replay")

	changed := request
	changed.SuccessURL = "https://greenfield.test/changed"
	_, err = client.CreateCheckoutSession(t.Context(), changed)
	require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
	require.EqualValues(t, 1, provider.checkoutCalls.Load(), "conflicting replay must not contact Stripe")

	lookup, err := client.LookupCheckoutSession(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first.ID, lookup.ID)

	before, err := client.CheckEntitlements(t.Context(), customer, []string{"content:premium"}, time.Time{})
	require.NoError(t, err)
	require.False(t, before["content:premium"])
	_, err = client.GrantEntitlement(t.Context(), customer, openrails.GrantEntitlementRequest{Entitlement: "content:premium"})
	require.NoError(t, err)
	after, err := client.CheckEntitlements(t.Context(), customer, []string{"content:premium"}, time.Time{})
	require.NoError(t, err)
	require.True(t, after["content:premium"], "the public access check observes the entitlement granted for the product")
}
