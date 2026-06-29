package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

func TestLiveStripeCatalogAutoCreateReusesContentKeys(t *testing.T) {
	if strings.TrimSpace(os.Getenv("OPENRAILS_LIVE_RAIL_TESTS")) != "1" {
		t.Skip("OPENRAILS_LIVE_RAIL_TESTS=1 required for live Stripe catalog test")
	}
	key := firstStripeLiveTestKey()
	if key == "" {
		t.Fatal("Stripe test key missing; set STRIPE_SECRET_KEY, RAILS_STRIPE_SECRET_KEY, or BILLING_RAILS_STRIPE_SECRET_KEY")
	}
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		t.Fatalf("refusing to run Stripe live catalog test without a Stripe test-mode key")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	stripeSvc := &catalog.StripeCatalogService{
		Config: &config.Config{Env: "dev", TestMode: true},
		Rails: config.RailSet{
			"stripe": {Type: config.RailTypeStripe, SecretKey: key},
		},
	}
	svc := &Service{rt: &app.Runtime{
		Config: &config.Config{Env: "dev", TestMode: true},
		Rails: config.RailSet{
			"stripe": {Type: config.RailTypeStripe, SecretKey: key},
		},
	}}
	adapter := &stripeAdapter{svc: svc}

	productID := uuid.New()
	priceID := uuid.New()
	cycle := 30
	slug := "live-stripe-catalog-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	product := &models.Product{
		ID:          productID,
		Slug:        slug,
		DisplayName: "OpenRails live Stripe catalog " + slug,
		Description: "OpenRails live Stripe catalog sync test",
		EntitlementsSpec: map[string]*int{
			"premium": nil,
		},
	}
	lookup := internalStripeLookupKey(slug, "usd", 12_340_000, &cycle)
	in := autoCreateContext{
		PriceID:          priceID,
		ProductID:        productID,
		Product:          product,
		ProductSlug:      slug,
		UnitAmount:       12_340_000,
		Currency:         "usd",
		BillingCycleDays: &cycle,
		LookupKey:        lookup,
	}

	first, err := adapter.AutoCreate(ctx, in)
	require.NoError(t, err)
	require.NotEmpty(t, first[models.RailKeyStripeProductID])
	require.NotEmpty(t, first[models.RailKeyStripePriceID])
	t.Cleanup(func() {
		inactive := false
		_ = stripeSvc.UpdatePrice(context.Background(), first[models.RailKeyStripePriceID], catalog.UpdatePriceParams{
			Active:         &inactive,
			IdempotencyKey: "openrails-live-cleanup-price-" + first[models.RailKeyStripePriceID],
		})
		_ = stripeSvc.UpdateProduct(context.Background(), first[models.RailKeyStripeProductID], catalog.UpdateProductParams{
			Active:         &inactive,
			IdempotencyKey: "openrails-live-cleanup-product-" + first[models.RailKeyStripeProductID],
		})
	})

	second, err := adapter.AutoCreate(ctx, in)
	require.NoError(t, err)
	require.Equal(t, first[models.RailKeyStripeProductID], second[models.RailKeyStripeProductID])
	require.Equal(t, first[models.RailKeyStripePriceID], second[models.RailKeyStripePriceID])
	require.Equal(t, lookup, second[providerLookupKey])

	remotePrice, err := stripeSvc.RetrievePrice(ctx, first[models.RailKeyStripePriceID])
	require.NoError(t, err)
	require.Equal(t, int64(1234), remotePrice.UnitAmount)
	require.Equal(t, "usd", strings.ToLower(remotePrice.Currency))
	require.Equal(t, lookup, remotePrice.LookupKey)
	require.NotNil(t, remotePrice.Recurring)
	require.Equal(t, "month", remotePrice.Recurring.Interval)
	require.Equal(t, 1, remotePrice.Recurring.Count)
	require.Equal(t, first[models.RailKeyStripeProductID], remotePrice.Product)

	oneTimePriceID := uuid.New()
	oneTimeLookup := internalStripeLookupKey(slug, "usd", 5_670_000, nil)
	oneTimeIn := autoCreateContext{
		PriceID:     oneTimePriceID,
		ProductID:   productID,
		Product:     product,
		ProductSlug: slug,
		UnitAmount:  5_670_000,
		Currency:    "usd",
		LookupKey:   oneTimeLookup,
	}
	oneTimeFirst, err := adapter.AutoCreate(ctx, oneTimeIn)
	require.NoError(t, err)
	require.Equal(t, first[models.RailKeyStripeProductID], oneTimeFirst[models.RailKeyStripeProductID])
	t.Cleanup(func() {
		inactive := false
		_ = stripeSvc.UpdatePrice(context.Background(), oneTimeFirst[models.RailKeyStripePriceID], catalog.UpdatePriceParams{
			Active:         &inactive,
			IdempotencyKey: "openrails-live-cleanup-price-" + oneTimeFirst[models.RailKeyStripePriceID],
		})
	})
	oneTimeSecond, err := adapter.AutoCreate(ctx, oneTimeIn)
	require.NoError(t, err)
	require.Equal(t, oneTimeFirst[models.RailKeyStripePriceID], oneTimeSecond[models.RailKeyStripePriceID])
	require.Equal(t, oneTimeLookup, oneTimeSecond[providerLookupKey])

	remoteOneTime, err := stripeSvc.RetrievePrice(ctx, oneTimeFirst[models.RailKeyStripePriceID])
	require.NoError(t, err)
	require.Equal(t, int64(567), remoteOneTime.UnitAmount)
	require.Nil(t, remoteOneTime.Recurring)
	require.Equal(t, oneTimeLookup, remoteOneTime.LookupKey)

	remoteProduct, err := stripeSvc.RetrieveProduct(ctx, first[models.RailKeyStripeProductID])
	require.NoError(t, err)
	require.Equal(t, slug, remoteProduct.Metadata[catalog.StripeMetadataOpenRailsProductKey])
	require.Equal(t, productBenefitFingerprint(product), remoteProduct.Metadata[catalog.StripeMetadataOpenRailsBenefitFingerprint])
}

func firstStripeLiveTestKey() string {
	for _, key := range []string{"STRIPE_SECRET_KEY", "RAILS_STRIPE_SECRET_KEY", "BILLING_RAILS_STRIPE_SECRET_KEY"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}
