//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// The money charged and the service credit granted are deliberately different.
// Exercise the public purchase API and registered renewal/expiry workers.
func TestRecurringCredits(t *testing.T) {
	a, provider := newPaymentWorld(t, func(w *world) { w.clock.Advance(-800 * 24 * time.Hour) })
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{
		Key: "monthly-credit", DisplayName: "Monthly service credits", Ownership: catalog.OwnershipUnique,
		CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", Amount: ptr(int64(25_000_000)), ExpiresAfterDays: ptr(365)},
	})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{
		ProductID: product.ID, Key: "monthly", UnitAmount: 20_000_000, Currency: "USD", BillingIntervalHours: ptr(720),
	})
	require.NoError(t, err)
	token := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(20_000_000)), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: token}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "monthly-credit-purchase"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status)
	require.NotNil(t, order.Lines[0].SubscriptionID)
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	balance(t, a.alpha, customer, 25_000_000, 0, 25_000_000)
	grants, err := a.alpha.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, grants.Items, 1)
	first := grants.Items[0]
	require.EqualValues(t, 25_000_000, first.Amount)
	require.Equal(t, first.StartsAt.AddDate(0, 0, 365), *first.ExpiresAt)
	require.Len(t, provider.effects(t), 1)
	require.EqualValues(t, 2000, provider.effects(t)[0].Amount, "Stripe charged $20, not the 25 service credits")

	sub, err := a.alpha.GetSubscription(t.Context(), *order.Lines[0].SubscriptionID)
	require.NoError(t, err)
	due := *sub.CurrentPeriodEndsAt
	require.Equal(t, 30*24*time.Hour, due.Sub(*sub.CurrentPeriodStartsAt))
	a.clock.Advance(due.Sub(a.clock.Now()))
	fleet := startFleet(t, a.world)
	runRegistered(t, fleet, "openrails.dunning", 2)
	require.Eventually(t, func() bool {
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		return err == nil && current.CurrentPeriodStartsAt != nil && current.CurrentPeriodStartsAt.Equal(due)
	}, 20*time.Second, 20*time.Millisecond)
	balance(t, a.alpha, customer, 50_000_000, 0, 50_000_000)
	runRegistered(t, fleet, "openrails.dunning", 2)
	balance(t, a.alpha, customer, 50_000_000, 0, 50_000_000)
	grants, err = a.alpha.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, grants.Items, 2, "each paid cycle funds exactly one credit lot")
	for _, grant := range grants.Items {
		require.EqualValues(t, 25_000_000, grant.Amount)
		require.Equal(t, grant.StartsAt.AddDate(0, 0, 365), *grant.ExpiresAt)
	}
	require.Len(t, provider.effects(t), 2)
	for _, effect := range provider.effects(t) {
		require.Equal(t, "charge", effect.Kind)
		require.EqualValues(t, 2000, effect.Amount)
	}

	status, raw = a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Stop future credit purchases"})
	require.Equal(t, http.StatusOK, status, string(raw))
	balance(t, a.alpha, customer, 50_000_000, 0, 50_000_000)
	a.clock.Advance(first.ExpiresAt.Sub(a.clock.Now()))
	runRegistered(t, fleet, "openrails.credit_expiry", 1)
	balance(t, a.alpha, customer, 25_000_000, 0, 25_000_000)
	runRegistered(t, fleet, "openrails.dunning", 2)
	require.Len(t, provider.effects(t), 2, "cancellation stops future payments without taking away paid credits")
}
