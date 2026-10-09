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

	// The fixture owns no migration files: New applies OpenRails' own, and
	// every client a test builds replays them.
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.NoError(t, err)
	require.NoError(t, client.Close(t.Context()))
	return f
}

// config is the fixture's engine configuration: its own schema for billing
// and River tables. Catalog updates stay unpublished: the in-process Client
// writes its own catalog as the process owner.
func (f *fixture) config() openrails.Config {
	return openrails.Config{
		Database:          openrails.DatabaseConfig{Schema: f.schema, RiverSchema: f.schema},
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
	deps := openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool}
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
		Key:          "welcome-" + uuid.NewString()[:8],
		DisplayName:  "Welcome",
		Entitlements: []string{"content:welcome"},
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

	forSale := true
	offers, err := client.ListProducts(t.Context(), billing.ProductListParams{Entitlements: []string{"content:welcome"}, ForSale: &forSale})
	require.NoError(t, err)
	require.Len(t, offers.Items, 1)
	require.Len(t, offers.Items[0].Prices, 1)
	require.Equal(t, price.ID, offers.Items[0].Prices[0].ID)
}

func TestMerchantCatalogAndCustomerIsolation(t *testing.T) {
	f := newFixture(t)
	alice := f.runtime(t, "merchant-a-"+uuid.NewString()[:8])
	bob := f.runtime(t, "merchant-b-"+uuid.NewString()[:8])

	productA, err := alice.CreateProduct(t.Context(), billing.CreateProductParams{Key: "alice-post", DisplayName: "Alice post", Entitlements: []string{"content:alice-post"}})
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
	_, err = alice.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: billing.CustomerID(uuid.MustParse(customerA)), ProductID: productA.ID}}})
	require.NoError(t, err)

	key := "content:" + productA.Key
	owned, err := heldKeys(t.Context(), alice, billing.CustomerID(uuid.MustParse(customerA)), time.Time{}, key)
	require.NoError(t, err)
	require.True(t, owned[key])
	foreign, err := heldKeys(t.Context(), bob, billing.CustomerID(uuid.MustParse(customerA)), time.Time{}, key)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{key: false}, foreign)
}

// A catalog application's meters and rate cards land in the runtime's own schema.
func TestCatalogApplicationSyncsMetersAndRateCards(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "metered-"+uuid.NewString()[:8])
	key := "metered-" + uuid.NewString()[:8]
	apply := func(unitAmount string) {
		params, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
meters:
  %[1]s-runtime:
    event_type: droplet.usage
    value_property: $.seconds
    aggregation: sum
products:
  %[1]s:
    display_name: Metered
    rate_cards:
    - meter: %[1]s-runtime
      price:
        model: per_unit
        currency: usd
        per_unit:
          unit_amount: "%[2]s"
`, key, unitAmount)))
		require.NoError(t, err)
		_, err = client.ApplyCatalog(t.Context(), params, billing.ApplyCatalogParams{})
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
		Key:          "premium-post-" + uuid.NewString()[:8],
		DisplayName:  "Premium post",
		Entitlements: []string{"content:premium"},
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
	session, err := sell(t, client, billing.CreateCheckoutSessionParams{
		Customer:   billing.CheckoutCustomerIdentity{ID: cid(customer), VerifiedEmail: "reader@example.test"},
		ProductKey: product.Key,
		PriceKey:   price.Key,
		SuccessURL: "https://e2e.test/success",
	})
	require.NoError(t, err)
	minted := session.read()
	require.EqualValues(t, 1_000_000, minted.Plan.UnitAmount)
	first, err := session.pay("stripe", nil)
	require.NoError(t, err)
	require.Equal(t, "requires_action", first.Status)
	require.NotNil(t, first.NextAction, "Stripe's hosted page is the next step: %+v", first)
	require.Equal(t, "redirect_to_url", first.NextAction.Type)
	require.Equal(t, "https://checkout.stripe.test/e2e", *first.NextAction.URL)
	attempt := session.attempt(f)

	replay, err := session.pay("stripe", nil)
	require.NoError(t, err)
	require.Equal(t, first, replay)
	require.Equal(t, attempt, session.attempt(f), "a repeated pay resumes the same attempt")
	require.EqualValues(t, 1, provider.checkoutCalls.Load(), "the provider sees one request across an identical replay")

	_, err = session.pay("stripe", map[string]any{"success_url": "https://e2e.test/changed"})
	require.ErrorIs(t, err, billing.ErrInvalid, "the page cannot change what the session was minted with")
	require.EqualValues(t, 1, provider.checkoutCalls.Load(), "a changed pay must not contact Stripe")

	read := session.read()
	require.Equal(t, "requires_action", read.Status)
	require.Equal(t, first.NextAction, read.NextAction)

	before, err := heldKeys(t.Context(), client, billing.CustomerID(uuid.MustParse(customer)), time.Time{}, "content:premium")
	require.NoError(t, err)
	require.False(t, before["content:premium"])
	_, err = client.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: billing.CustomerID(uuid.MustParse(customer)), ProductID: product.ID}}})
	require.NoError(t, err)
	after, err := heldKeys(t.Context(), client, billing.CustomerID(uuid.MustParse(customer)), time.Time{}, "content:premium")
	require.NoError(t, err)
	require.True(t, after["content:premium"], "the public access check observes the entitlement granted for the product")
}

// recordUsage records one usage event; the item's refusal is the error.
func recordUsage(ctx context.Context, client *openrails.Client, params billing.RecordUsageParams) (*billing.UsageEvent, error) {
	results, err := client.RecordUsage(ctx, []billing.RecordUsageParams{params})
	if err != nil {
		return nil, err
	}
	return results[0].Event, results[0].Err()
}

// createCreditGrant grants one customer credit: a batch of one.
func createCreditGrant(ctx context.Context, client *openrails.Client, customer billing.CustomerID, params billing.CreateCreditGrantParams) (*billing.CreditGrant, error) {
	params.CustomerID = customer
	grants, err := client.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{params})
	if err != nil {
		return nil, err
	}
	return &grants[0], nil
}

// releaseAdmission releases one admission: a batch of one.
func releaseAdmission(ctx context.Context, client *openrails.Client, requestID string) (*billing.Admission, error) {
	results, err := client.ReleaseAdmissions(ctx, []string{requestID})
	if err != nil {
		return nil, err
	}
	return results[0].Admission, results[0].Err()
}

// extendAdmission extends one hold: a batch of one.
func extendAdmission(ctx context.Context, client *openrails.Client, requestID string, expiresAt time.Time) (*billing.Admission, error) {
	results, err := client.ExtendAdmissions(ctx, []billing.ExtendAdmissionParams{{RequestID: requestID, ExpiresAt: expiresAt}})
	if err != nil {
		return nil, err
	}
	return results[0].Admission, results[0].Err()
}
