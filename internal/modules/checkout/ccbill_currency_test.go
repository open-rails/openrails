package checkout

import (
	"context"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

func ccbillCheckoutService(t *testing.T) (context.Context, *CheckoutService) {
	t.Helper()
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.PSPSecretName("ccbill", "live", "945280-0000", "salt")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-salt")
	require.NoError(t, err)

	svc := &CheckoutService{Config: checkoutRailConfig(true), Rails: checkoutRailSet("static-mobius-key")}
	svc.SetMerchantSecretStore(store)
	svc.SetPSPSecretResolver(checkoutStaticProviderSecretResolver{
		rail:        "ccbill",
		environment: "live",
		accountID:   "945280-0000",
	})
	return ctx, svc
}

func ccbillPrice(currency string) *models.Price {
	return &models.Price{
		ID:       uuid.New(),
		Amount:   19_990_000,
		Currency: currency,
		PSPLinks: map[string]map[string]string{
			"ccbill": {
				"rail":      "ccbill",
				"form_name": "premium",
				"flex_id":   "flex-123",
			},
		},
	}
}

func ccbillTestUser() *UserIdentity {
	email := "alice@example.com"
	return &UserIdentity{ID: uuid.NewString(), Username: "alice", Email: &email}
}

// #819: a EUR-priced product must reach CCBill AS EUR. Before the fix the
// FlexForm always carried currencyCode=840 (USD), so the customer was charged
// $19.99 for a €19.99 product and the webhook then rejected the membership on
// the currency mismatch.
func TestCCBillCheckoutSendsThePriceCurrency(t *testing.T) {
	ctx, svc := ccbillCheckoutService(t)

	resp, err := svc.processCCBillSubscription(ctx, &CheckoutRequest{}, ccbillTestUser(), ccbillPrice("eur"))
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "978", parsed.Query().Get("currencyCode"))
}

// A currency CCBill cannot bill is refused at checkout — before the customer
// ever reaches a payment form, therefore before any charge.
func TestCCBillCheckoutRefusesUnbillableCurrency(t *testing.T) {
	ctx, svc := ccbillCheckoutService(t)

	resp, err := svc.processCCBillSubscription(ctx, &CheckoutRequest{}, ccbillTestUser(), ccbillPrice("sek"))
	require.Error(t, err)
	require.Nil(t, resp)
	require.Contains(t, err.Error(), "sek")
}

// A price with no currency at all is refused too: a missing currency is never
// defaulted or invented (docs/invariants.md).
func TestCCBillCheckoutRefusesMissingCurrency(t *testing.T) {
	ctx, svc := ccbillCheckoutService(t)

	resp, err := svc.processCCBillSubscription(ctx, &CheckoutRequest{}, ccbillTestUser(), ccbillPrice(""))
	require.Error(t, err)
	require.Nil(t, resp)
}

func TestCCBillUpgradeSendsThePriceCurrency(t *testing.T) {
	ctx, svc := ccbillCheckoutService(t)

	sub := &models.Subscription{
		ID:                 uuid.New(),
		Rail:               models.RailCCBill,
		RailSubscriptionID: "ccbill-sub-1",
		PriceID:            uuid.New(),
	}
	resp, err := svc.processCCBillUpgrade(ctx, ccbillTestUser(), ccbillPrice("eur"), sub)
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "978", parsed.Query().Get("currencyCode"))
}
