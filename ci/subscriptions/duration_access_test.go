//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

func enrollAccessDuration(t *testing.T, w *world, rail string, tp topology, access *int) *engineCase {
	t.Helper()
	product, err := w.client[tp].CreateProduct(t.Context(), billing.CreateProductParams{
		Key: "duration-" + uuid.NewString(), DisplayName: "Independent access",
		Entitlements: []string{"content:duration"},
	})
	require.NoError(t, err)
	price, err := w.client[tp].CreatePrice(t.Context(), billing.CreatePriceParams{
		ProductID: product.ID, Key: "monthly", UnitAmount: 9_990_000, Currency: "USD",
		BillingIntervalHours: new(720), AccessDurationHours: access,
	})
	require.NoError(t, err)
	e := &engineCase{w: w, rail: rail, tp: tp, price: price.ID.String(), amount: 999, ent: "content:duration", started: w.clock.Now()}
	e.c = w.newCustomer()
	e.method = e.c.saveCard(rail, visa)
	// The merchant creates the order through tp's Client; its customer pays
	// it signed in with the saved card.
	paid, err := e.c.buyOrder(tp, order{price: price.ID, rail: rail, method: e.method})
	require.NoError(t, err)
	require.Equal(t, "succeeded", paid.Status, "%+v", paid.Order)
	require.NotNil(t, paid.SubscriptionID)
	e.sub = *paid.SubscriptionID
	w.settle()
	require.Len(t, e.providerLedger(), 1, "one initial payment")
	require.True(t, e.c.entitled(e.ent))
	require.True(t, e.started.Add(720*time.Hour).Equal(e.periodEnd()), "billing uses its interval independently of access")
	return e
}

func requireDurationSelfAccess(t *testing.T, e *engineCase, want bool) {
	t.Helper()
	got := unwrap(e.c.must(http.MethodGet, "/subscriptions/"+e.sub.String(), "", nil))
	require.Equal(t, want, got["access"] != nil, "self subscription detail reflects the live grant ledger")
	list := decodeSubs(t, e.c.must(http.MethodGet, "/subscriptions", "", nil))
	for _, sub := range list {
		if sub.ID == e.sub {
			require.Equal(t, want, sub.Access != nil, "self subscription list reflects the live grant ledger")
			return
		}
	}
	t.Fatal("self subscription list omitted the membership")
}

func TestDurationAccessExpiresBeforeNextBill(t *testing.T) {
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		e := enrollAccessDuration(t, w, rail, tp, new(72))
		w.advance(73 * time.Hour)
		w.runRenewals()
		w.converge()
		require.False(t, e.c.entitled(e.ent), "being active does not reopen expired paid access")
		requireDurationSelfAccess(t, e, false)
		require.Equal(t, billing.SubscriptionActive, w.subscription(tp, e.sub).Status)
		require.Len(t, e.providerLedger(), 1, "access expiry cannot trigger an early renewal")

		e.toFreshPeriodEnd()
		w.runRenewals()
		w.runRenewals()
		require.Len(t, e.providerLedger(), 2, "one charge at the billing boundary")
		require.True(t, e.c.entitled(e.ent), "the new charge grants a new access window")
		requireDurationSelfAccess(t, e, true)
		_, err := w.client[tp].CancelSubscription(t.Context(), e.sub, billing.CancelSubscriptionParams{Reason: "finished"})
		require.NoError(t, err)
		w.advance(73 * time.Hour)
		w.converge()
		require.False(t, e.c.entitled(e.ent), "cancellation and reconciliation keep the purchased expiry")
		w.advance(720 * time.Hour)
		w.runRenewals()
		require.Len(t, e.providerLedger(), 2, "a canceled subscription never charges again")
	})
}

func TestDurationAccessSurvivesCancellation(t *testing.T) {
	// A recurring price's access is finite (#1275): a longer window than the
	// billing interval outlives cancellation and then expires on its own.
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		e := enrollAccessDuration(t, w, rail, tp, new(1440))
		_, err := w.client[tp].CancelSubscription(t.Context(), e.sub, billing.CancelSubscriptionParams{Reason: "keep purchased access"})
		require.NoError(t, err)
		w.advance(721 * time.Hour)
		w.runRenewals()
		w.converge()
		require.True(t, e.c.entitled(e.ent), "billing cancellation does not shorten purchased access")
		requireDurationSelfAccess(t, e, true)
		require.Len(t, e.providerLedger(), 1)
		w.advance(721 * time.Hour)
		w.runRenewals()
		w.converge()
		require.False(t, e.c.entitled(e.ent), "finite access expires on its own boundary")
		requireDurationSelfAccess(t, e, false)
		require.Len(t, e.providerLedger(), 1)
	})
}
