package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestCatalogAccessAndBillingAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		access  *int
		billing *int
	}{
		{"one-time perpetual", nil, nil},
		{"one-time finite", intPtr(72), nil},
		{"recurring perpetual", nil, intPtr(720)},
		{"recurring finite", intPtr(72), intPtr(720)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := billing.CreatePriceParams{Currency: "USD", UnitAmount: 100_000_000, AccessDurationHours: tc.access, BillingIntervalHours: tc.billing}
			require.NoError(t, validateCatalogPriceTerms(req))
			if tc.billing == nil {
				require.Nil(t, priceRequestCycleDays(req))
			} else {
				require.Equal(t, 30, *priceRequestCycleDays(req), "provider cadence must ignore the access window")
			}
		})
	}
	for _, hours := range []int{0, -1, catalogwire.MaxDurationHours + 1} {
		require.Error(t, validateCatalogPriceTerms(billing.CreatePriceParams{Currency: "USD", AccessDurationHours: intPtr(hours)}))
		require.Error(t, validateCatalogPriceTerms(billing.CreatePriceParams{Currency: "USD", BillingIntervalHours: intPtr(hours)}))
	}
	require.ErrorContains(t, validateCatalogPriceTerms(billing.CreatePriceParams{Currency: "USD", TrialUnitAmount: int64Ptr(0), TrialDurationHours: intPtr(24)}), "billing_interval_hours")
}

func TestCatalogDurationUpdatePreservesOmittedAndClearsNull(t *testing.T) {
	product := &billing.Product{ID: billing.ProductID(uuid.New()), Key: "product"}
	current := billing.Price{ID: billing.PriceID(uuid.New()), ProductID: product.ID, Key: "price", Currency: "USD", UnitAmount: 10_000_000,
		AccessDurationHours: intPtr(72), BillingIntervalHours: intPtr(720)}
	byKey := map[string][]billing.Price{"price": {current}}
	for _, tc := range []struct {
		name    string
		decl    catalogwire.ApplyPrice
		access  *int
		billing *int
	}{
		{"omitted", catalogwire.ApplyPrice{Key: "price"}, current.AccessDurationHours, current.BillingIntervalHours},
		{"clear access", catalogwire.ApplyPrice{Key: "price", AccessDurationHours: catalogwire.Null[int]()}, nil, current.BillingIntervalHours},
		{"clear billing", catalogwire.ApplyPrice{Key: "price", BillingIntervalHours: catalogwire.Null[int]()}, current.AccessDurationHours, nil},
		{"change billing", catalogwire.ApplyPrice{Key: "price", BillingIntervalHours: catalogwire.Value(24)}, current.AccessDurationHours, intPtr(24)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, req, err := catalogApplicationPriceRequest(product, tc.decl, byKey, nil)
			require.NoError(t, err)
			require.Equal(t, tc.access, req.AccessDurationHours)
			require.Equal(t, tc.billing, req.BillingIntervalHours)
		})
	}
	_, created, err := catalogApplicationPriceRequest(product, catalogwire.ApplyPrice{Key: "new", Currency: catalogwire.Value("USD"), UnitAmount: catalogwire.Value[int64](10_000_000), AccessDurationHours: catalogwire.Value(72)}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, intPtr(72), created.AccessDurationHours)
	require.Nil(t, created.BillingIntervalHours, "a finite access window must not create recurring billing")
	_, created, err = catalogApplicationPriceRequest(product, catalogwire.ApplyPrice{Key: "new", Currency: catalogwire.Value("USD"), UnitAmount: catalogwire.Value[int64](10_000_000), BillingIntervalHours: catalogwire.Value(720)}, nil, nil)
	require.NoError(t, err)
	require.Nil(t, created.AccessDurationHours, "recurring billing must not create an access expiry")
	require.Equal(t, intPtr(720), created.BillingIntervalHours)
}

func TestCatalogProvidersRejectFractionalDayBilling(t *testing.T) {
	terms := autoCreateContext{BillingIntervalHours: intPtr(25), BillingCycleDays: intPtr(1)}
	for _, adapter := range []providerAdapter{&stripeAdapter{}, &nmiAdapter{}} {
		_, err := adapter.AutoCreate(t.Context(), terms)
		require.ErrorContains(t, err, "whole-day billing interval")
		_, err = adapter.Attach(t.Context(), map[string]string{}, terms)
		require.ErrorContains(t, err, "whole-day billing interval")
	}
}
