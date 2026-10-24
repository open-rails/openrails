//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func purchasedMembership(t *testing.T, intervalHours ...int) (*authorityWorld, *providerLedger, *billing.Subscription) {
	t.Helper()
	hours := 24
	if len(intervalHours) > 0 {
		hours = intervalHours[0]
	}
	// Keep accelerated business periods in the past: PostgreSQL correctly
	// refuses payment observations dated in its future. AuthKit sign-in uses
	// its actual clock and remains independent of these financial periods.
	a, provider := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "membership", DisplayName: "Membership", Entitlements: []string{"membership:paid"}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "membership-day", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(hours), AccessDurationHours: ptr(hours)})
	require.NoError(t, err)
	token := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: token}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "membership-purchase"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status)
	require.Len(t, order.Lines, 1)
	require.NotNil(t, order.Lines[0].SubscriptionID)
	require.NotNil(t, order.Payment.PaymentID)
	payment, err := a.alpha.GetPayment(t.Context(), *order.Payment.PaymentID)
	require.NoError(t, err)
	require.EqualValues(t, 10_000_000, payment.Amount)
	require.Equal(t, billing.PaymentSucceeded, payment.Status)
	sub, err := a.alpha.GetSubscription(t.Context(), *order.Lines[0].SubscriptionID)
	require.NoError(t, err)
	require.NotNil(t, sub.CurrentPeriodStartsAt)
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	require.Equal(t, time.Duration(hours)*time.Hour, sub.CurrentPeriodEndsAt.Sub(*sub.CurrentPeriodStartsAt))
	effects := provider.effects(t)
	require.Len(t, effects, 1)
	require.Equal(t, "charge", effects[0].Kind)
	require.EqualValues(t, 1000, effects[0].Amount)
	return a, provider, sub
}

func TestSubscriptions(t *testing.T) {
	t.Run("OR-J13-S01/paid-enrollment-has-one-charge-and-period", func(t *testing.T) {
		a, provider, sub := purchasedMembership(t)
		require.Equal(t, "active", string(sub.Status))
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		require.Equal(t, customer, sub.CustomerID)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["membership:paid"])
		facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
		require.NoError(t, err)
		settled, completed := 0, 0
		for _, event := range facts.Events {
			if event.Type == "payment.settled" {
				settled++
			}
			if event.Type == "order.completed" {
				completed++
			}
		}
		require.Equal(t, 1, settled)
		require.Equal(t, 1, completed)
		require.Empty(t, provider.core.Unexpected())
	})
	t.Run("OR-J17-S01/cancel-resume-keeps-the-purchased-window", func(t *testing.T) {
		a, provider, sub := purchasedMembership(t)
		end := *sub.CurrentPeriodEndsAt
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Customer pauses renewal"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var canceled billing.Subscription
		require.NoError(t, json.Unmarshal(raw, &canceled))
		require.True(t, canceled.Resumable, "an ordinary customer cancellation remains reversible within the paid period")
		require.Equal(t, "canceled", string(canceled.Status))
		require.Equal(t, end, *canceled.CurrentPeriodEndsAt)
		for _, sample := range []struct {
			at      time.Time
			allowed bool
		}{{a.clock.Now(), true}, {end.Add(-time.Microsecond), true}, {end, false}} {
			access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}, At: sample.at})
			require.NoError(t, err)
			require.Equal(t, sample.allowed, access.Entitlements["membership:paid"], sample.at.String())
		}
		resumed, err := a.alpha.ResumeSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, "active", string(resumed.Status))
		require.Equal(t, end, *resumed.CurrentPeriodEndsAt)
		require.Len(t, provider.effects(t), 1, "cancel/resume inside the paid period does not buy another period")
		// A merchant revocation is a different decision and may not be undone
		// by treating it as an ordinary customer pause.
		adminCanceled, err := a.alpha.CancelSubscription(t.Context(), sub.ID, billing.CancelSubscriptionParams{Reason: "Merchant ends future service"})
		require.NoError(t, err)
		require.False(t, adminCanceled.Resumable)
		status, raw = a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/resume", a.aliceToken, map[string]any{})
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"invalid_param"`)
		require.Len(t, provider.effects(t), 1)

	})
}
