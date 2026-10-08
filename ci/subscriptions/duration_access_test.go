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

func enrollAccessDuration(t *testing.T, w *world, rail string, tp topology, access *int, autoRenew *bool) (*engineCase, billing.CreateCheckoutAttemptParams) {
	t.Helper()
	product, err := w.client[tp].CreateProduct(t.Context(), billing.CreateProductParams{
		Key: "duration-" + uuid.NewString(), DisplayName: "Independent access",
		EntitlementsSpec: map[string]*int{"content:duration": nil},
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
	params := billing.CreateCheckoutAttemptParams{
		OfferKind: billing.OfferRecurring, Customer: e.c.identity(), Entitlement: e.ent, PriceID: price.ID,
		AutoRenew: autoRenew, IdempotencyKey: "duration-" + uuid.NewString(),
		PaymentOptions: billing.CheckoutPaymentOptions{PSP: rail, PaymentMethodID: pmid(e.method)},
		SuccessURL:     "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
	}
	attempt, err := w.client[tp].CreateCheckoutAttempt(t.Context(), params)
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutAttemptSucceeded, attempt.Status)
	require.NotNil(t, attempt.SubscriptionID)
	e.sub = *attempt.SubscriptionID
	w.settle()
	require.Len(t, e.providerLedger(), 1, "one initial payment")
	require.True(t, e.c.entitled(e.ent))
	require.True(t, e.started.Add(720*time.Hour).Equal(e.periodEnd()), "billing uses its interval independently of access")
	return e, params
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
		e, _ := enrollAccessDuration(t, w, rail, tp, new(72), nil)
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
	for _, access := range []struct {
		name  string
		hours *int
	}{{"longer_than_billing", new(1440)}, {"indefinite", nil}} {
		t.Run(access.name, func(t *testing.T) {
			forEach(t, func(t *testing.T, rail string, tp topology) {
				w := newWorld(t)
				e, _ := enrollAccessDuration(t, w, rail, tp, access.hours, nil)
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
				require.Equal(t, access.hours == nil, e.c.entitled(e.ent), "finite access expires on its own boundary; indefinite access remains")
				requireDurationSelfAccess(t, e, access.hours == nil)
				if access.hours == nil {
					w.refreshProviders()
					_, err = w.client[tp].CancelSubscription(t.Context(), e.sub, billing.CancelSubscriptionParams{Reason: "revoke purchased access", RevokeAccess: true})
					require.NoError(t, err)
					require.False(t, e.c.entitled(e.ent))
					requireDurationSelfAccess(t, e, false)
				}
				require.Len(t, e.providerLedger(), 1)
			})
		})
	}
}

func TestDurationOrderCanDisableRenewalAtCreation(t *testing.T) {
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		e, params := enrollAccessDuration(t, w, rail, tp, new(72), new(false))
		sub := w.subscription(tp, e.sub)
		require.Equal(t, billing.SubscriptionCanceled, sub.Status, "one command records the paid order without a later renewal")
		require.NotNil(t, sub.CanceledAt)
		require.Nil(t, sub.NextRetryAt)
		replayed, err := w.client[tp].CreateCheckoutAttempt(t.Context(), params)
		require.NoError(t, err)
		require.Equal(t, e.sub, *replayed.SubscriptionID)
		require.Len(t, e.providerLedger(), 1, "same order replay cannot charge twice")
		params.AutoRenew = new(true)
		_, err = w.client[tp].CreateCheckoutAttempt(t.Context(), params)
		require.Error(t, err, "an idempotency key cannot change the accepted renewal preference")
		require.Len(t, e.providerLedger(), 1)
		w.advance(73 * time.Hour)
		w.converge()
		require.False(t, e.c.entitled(e.ent))
		w.advance(720 * time.Hour)
		w.runRenewals()
		w.runRenewals()
		require.Len(t, e.providerLedger(), 1, "auto_renew:false never queues another payment")
		require.Equal(t, 1, e.providerAttempts())
	})
}

func TestDurationOrderWithoutRenewalRefusesUnsupportedTrialBeforeCharge(t *testing.T) {
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		product, err := w.client[tp].CreateProduct(t.Context(), billing.CreateProductParams{Key: "trial-order", DisplayName: "Trial order", EntitlementsSpec: map[string]*int{"content:trial-order": nil}})
		require.NoError(t, err)
		price, err := w.client[tp].CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 9_990_000, Currency: "USD", BillingIntervalHours: new(720), AccessDurationHours: new(24), TrialUnitAmount: new(int64(0)), TrialDurationHours: new(24)})
		require.NoError(t, err)
		e := &engineCase{w: w, rail: rail, tp: tp}
		e.c = w.newCustomer()
		e.method = e.c.saveCard(rail, visa)
		_, err = w.client[tp].CreateCheckoutAttempt(t.Context(), billing.CreateCheckoutAttemptParams{
			OfferKind: billing.OfferRecurring, Customer: e.c.identity(), Entitlement: "content:trial-order", PriceID: price.ID,
			AutoRenew: new(false), IdempotencyKey: "trial-order-" + uuid.NewString(),
			PaymentOptions: billing.CheckoutPaymentOptions{PSP: rail, PaymentMethodID: pmid(e.method)},
			SuccessURL:     "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
		})
		require.Error(t, err, "engine trial checkout requires supported trial settlement")
		require.Empty(t, e.providerLedger(), "an unsupported trial cannot turn into a paid recurring purchase")
		require.Zero(t, e.providerAttempts())
		w.advance(721 * time.Hour)
		w.runRenewals()
		require.Empty(t, e.providerLedger(), "there is no later trial conversion charge")
	})
}
