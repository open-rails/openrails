//go:build e2e && integration

// Package ci contains the first replacement CI slice. It deliberately
// talks only to the public embedded API: the old integration harness and
// application tables are not part of this fixture.
package ci_test

import (
	"context"
	"fmt"
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
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

type fixture struct {
	pool   *pgxpool.Pool
	schema string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	if dsn == "" {
		t.Fatal("OPENRAILS_E2E_DSN must point at a disposable PostgreSQL database")
	}

	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(t.Context()))

	f := &fixture{pool: pool, schema: "e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = f.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
		f.pool.Close()
	})

	// The fixture owns no migration files. OpenRails applies its own public
	// migrations, and the second call proves replay is safe before a client is
	// constructed.
	for range 2 {
		require.NoError(t, openrails.Migrate(t.Context(), pool, f.config()))
	}
	return f
}

// config is the fixture's engine configuration: its own schema for billing
// tables and managed River. Catalog updates stay unpublished: the in-process
// Client writes its own catalog as the process owner.
func (f *fixture) config() openrails.Config {
	return openrails.Config{
		Schema:            f.schema,
		RiverSchema:       f.schema,
		TestMode:          openrails.Sandbox,
		ProviderWriteMode: openrails.ProviderWritesReadOnly,
		ReturnOrigins:     []string{"https://e2e.test"},
	}
}

func (f *fixture) runtime(t *testing.T, slug string) *openrails.Client {
	return f.runtimeWithStripe(t, slug, nil)
}

func (f *fixture) runtimeWithStripe(t *testing.T, slug string, transport http.RoundTripper) *openrails.Client {
	t.Helper()
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: slug, DisplayName: slug}
	deps := openrails.Deps{Postgres: f.pool}
	if transport != nil {
		cfg.ProviderWriteMode = openrails.ProviderWritesFull
		deps.StripeTransport = transport
		cfg.Merchant.PSPs = map[string]openrails.PSPConfig{"stripe": {Rail: "stripe",
			AccountID: "acct_e2e",
			Secrets: map[string]string{
				"secret_key":             "sk_test_e2e",
				"webhook_signing_secret": "whsec_e2e",
			},
		}}
	}
	client, err := openrails.New(t.Context(), cfg, deps)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	return client
}

func (f *fixture) dsn(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	return dsn
}

func TestFreshBootstrapAndReplay(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "bootstrap-"+uuid.NewString()[:8])

	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{
		Key:              "welcome-" + uuid.NewString()[:8],
		DisplayName:      "Welcome",
		EntitlementsSpec: map[string]*int{"content:welcome": nil},
	})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{
		ProductID:  product.ID,
		Key:        "welcome-usd-" + uuid.NewString()[:8],
		UnitAmount: 9_007_199_254_740_993,
		Currency:   "USD",
	})
	require.NoError(t, err)
	require.Equal(t, product.ID, price.ProductID)
	require.EqualValues(t, 9_007_199_254_740_993, price.UnitAmount)
	stored, err := client.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Equal(t, price.UnitAmount, stored.UnitAmount, "PostgreSQL and the embedded API preserve amounts above 2^53")

	offers, err := client.ListOffers(t.Context(), billing.OfferListParams{Entitlements: []string{"content:welcome"}, Kind: billing.OfferPermanent})
	require.NoError(t, err)
	require.Len(t, offers["content:welcome"].Items, 1)
	require.Equal(t, price.ID, offers["content:welcome"].Items[0].PriceID)
}

func TestMerchantCatalogAndCustomerIsolation(t *testing.T) {
	f := newFixture(t)
	alice := f.runtime(t, "merchant-a-"+uuid.NewString()[:8])
	bob := f.runtime(t, "merchant-b-"+uuid.NewString()[:8])

	productA, err := alice.CreateProduct(t.Context(), billing.CreateProductParams{Key: "alice-post", DisplayName: "Alice post"})
	require.NoError(t, err)
	productB, err := bob.CreateProduct(t.Context(), billing.CreateProductParams{Key: "bob-post", DisplayName: "Bob post"})
	require.NoError(t, err)

	_, err = alice.GetProduct(t.Context(), productB.ID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	page, err := alice.ListProducts(t.Context(), billing.ProductListParams{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, productA.ID, page.Items[0].ID)

	customerA := uuid.NewString()
	customerB := uuid.NewString()
	_, err = alice.EnsureCustomer(t.Context(), billing.CustomerID(uuid.MustParse(customerA)), billing.EnsureCustomerParams{})
	require.NoError(t, err)
	_, err = bob.EnsureCustomer(t.Context(), billing.CustomerID(uuid.MustParse(customerB)), billing.EnsureCustomerParams{})
	require.NoError(t, err)
	_, err = alice.CreateEntitlement(t.Context(), billing.CustomerID(uuid.MustParse(customerA)), billing.CreateEntitlementParams{Entitlement: "content:" + productA.Key})
	require.NoError(t, err)

	owned, err := alice.HasEntitlement(t.Context(), billing.CustomerID(uuid.MustParse(customerA)), "content:"+productA.Key, time.Time{})
	require.NoError(t, err)
	require.True(t, owned)
	foreign, err := bob.HasEntitlement(t.Context(), billing.CustomerID(uuid.MustParse(customerA)), "content:"+productA.Key, time.Time{})
	require.NoError(t, err)
	require.False(t, foreign)
}

func TestCatalogEnsureIsIdempotent(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "idempotent-"+uuid.NewString()[:8])
	key := "stable-product-" + uuid.NewString()[:8]

	first, err := client.EnsureProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: "First title"})
	require.NoError(t, err)
	second, err := client.EnsureProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: "Changed title"})
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.DisplayName, second.DisplayName)

	read, err := client.GetProductByKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, first.ID, read.ID)
	require.Equal(t, first.DisplayName, read.DisplayName)
}

// A catalog application's meters and rate cards land in the runtime's own schema.
func TestCatalogApplicationSyncsMetersAndRateCards(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "metered-"+uuid.NewString()[:8])
	key := "metered-" + uuid.NewString()[:8]
	apply := func(unitAmount string) {
		revision, err := client.GetCatalogRevision(t.Context())
		require.NoError(t, err)
		params, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
application_id: gf-%[1]s-%[3]s
expected_revision: %[2]d
meters:
- key: %[1]s-runtime
  event_type: droplet.usage
  value_property: $.seconds
  aggregation: sum
products:
- key: %[1]s
  display_name: Metered
  rate_cards:
  - meter: %[1]s-runtime
    price:
      model: per_unit
      currency: usd
      per_unit:
        unit_amount: "%[3]s"
`, key, revision.Revision, unitAmount)))
		require.NoError(t, err)
		_, err = client.ApplyCatalog(t.Context(), params)
		require.NoError(t, err)
	}
	apply("10")
	apply("20") // overwrites the stored card in place

	meter, err := client.GetMeter(t.Context(), key+"-runtime")
	require.NoError(t, err)
	require.Equal(t, "droplet.usage", meter.EventType)
	require.NotNil(t, meter.RateCard)
	require.Equal(t, key, meter.RateCard.ProductKey)
	require.NotNil(t, meter.RateCard.Price.PerUnit)
	require.EqualValues(t, 20, meter.RateCard.Price.PerUnit.UnitAmount)
}

func TestCheckoutReplayAndEntitlementAccess(t *testing.T) {
	f := newFixture(t)
	provider := &stripeCheckoutFake{t: t}
	client := f.runtimeWithStripe(t, "checkout-"+uuid.NewString()[:8], provider)

	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{
		Key:              "premium-post-" + uuid.NewString()[:8],
		DisplayName:      "Premium post",
		EntitlementsSpec: map[string]*int{"content:premium": nil},
	})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{
		ProductID:  product.ID,
		Key:        product.Key + "-usd",
		UnitAmount: 1_000_000,
		Currency:   "USD",
	})
	require.NoError(t, err)

	customer := uuid.NewString()
	request := billing.CreateCheckoutAttemptParams{
		Customer:       billing.CheckoutCustomerIdentity{ID: cid(customer), VerifiedEmail: "reader@example.test"},
		PriceKey:       price.Key,
		Entitlement:    "content:premium",
		OfferKind:      billing.OfferPermanent,
		PaymentOptions: billing.CheckoutPaymentOptions{PSP: "stripe"},
		IdempotencyKey: "checkout-" + uuid.NewString(),
		SuccessURL:     "https://e2e.test/success",
		CancelURL:      "https://e2e.test/cancel",
	}
	first, err := client.CreateCheckoutAttempt(t.Context(), request)
	require.NoError(t, err)
	require.NotNil(t, first.NextAction, "Stripe's hosted page is the next step: %+v", first)
	require.Equal(t, "redirect_to_url", first.NextAction.Type)
	require.Equal(t, "https://checkout.stripe.test/e2e", *first.NextAction.URL)
	require.NotNil(t, first.PriceID)
	require.Equal(t, price.ID, *first.PriceID)

	replay, err := client.CreateCheckoutAttempt(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first.ID, replay.ID)
	require.NotNil(t, first.Amount)
	require.EqualValues(t, 1_000_000, *first.Amount)
	require.Equal(t, first.Amount, replay.Amount)
	require.EqualValues(t, 1, provider.checkoutCalls.Load(), "the provider sees one request across an identical replay")

	changed := request
	changed.SuccessURL = "https://e2e.test/changed"
	_, err = client.CreateCheckoutAttempt(t.Context(), changed)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	require.EqualValues(t, 1, provider.checkoutCalls.Load(), "conflicting replay must not contact Stripe")

	read, err := client.GetCheckoutAttempt(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, first.ID, read.ID)
	require.Equal(t, request.Customer.ID, read.CustomerID)

	before, err := client.HasEntitlement(t.Context(), billing.CustomerID(uuid.MustParse(customer)), "content:premium", time.Time{})
	require.NoError(t, err)
	require.False(t, before)
	_, err = client.CreateEntitlement(t.Context(), billing.CustomerID(uuid.MustParse(customer)), billing.CreateEntitlementParams{Entitlement: "content:premium"})
	require.NoError(t, err)
	after, err := client.HasEntitlement(t.Context(), billing.CustomerID(uuid.MustParse(customer)), "content:premium", time.Time{})
	require.NoError(t, err)
	require.True(t, after, "the public access check observes the entitlement granted for the product")
}
