//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func upgradePrice(t *testing.T, a *authorityWorld, sub *billing.Subscription) *billing.Price {
	t.Helper()
	_, err := a.alpha.UpdateProduct(t.Context(), sub.ProductID, billing.UpdateProductParams{TierGroup: catalog.Value("membership-level"), TierRank: catalog.Value(1)})
	require.NoError(t, err)
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "higher-membership", DisplayName: "Higher membership", TierGroup: ptr("membership-level"), TierRank: 2, Entitlements: []string{"membership:higher"}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 20_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
	require.NoError(t, err)
	return price
}
func TestSubscriptionChangeContracts(t *testing.T) {
	t.Run("OR-J18-S01/staff-preview-and-scheduled-change-agree-at-renewal", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		price := upgradePrice(t, a, sub)
		params := billing.ChangeSubscriptionParams{PriceID: &price.ID, Reason: "Customer requested larger plan", IdempotencyKey: "scheduled-upgrade"}
		preview, err := a.alpha.PreviewSubscriptionChange(t.Context(), sub.ID, params)
		require.NoError(t, err)
		require.Equal(t, "period_end", preview.Effective)
		require.EqualValues(t, 0, preview.AmountDueNow)
		require.EqualValues(t, 20_000_000, preview.NextChargeAmount)
		require.Equal(t, sub.CurrentPeriodEndsAt, preview.NextChargeDate)
		// Staff are offered the plan with the staff change's own preview.
		options, err := a.alpha.ListSubscriptionChangeOptions(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Len(t, options.Items, 1)
		require.Equal(t, price.ID, options.Items[0].Price.ID)
		require.Equal(t, *preview, options.Items[0].Preview)
		changed, err := a.alpha.ChangeSubscription(t.Context(), sub.ID, params)
		require.NoError(t, err)
		require.Equal(t, preview.Effective, changed.Effective)
		require.Equal(t, preview.AmountDueNow, changed.AmountDueNow)
		require.Equal(t, preview.NextChargeAmount, changed.NextChargeAmount)
		repeat, err := a.alpha.ChangeSubscription(t.Context(), sub.ID, params)
		require.NoError(t, err)
		require.Equal(t, changed, repeat)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, sub.PriceID, current.PriceID)
		require.NotNil(t, current.ScheduledChange)
		require.Equal(t, price.ID, current.ScheduledChange.PriceID)
		require.Len(t, p.effects(t), 1)
		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.PriceID == price.ID && current.CurrentPeriodStartsAt.Equal(due)
		}, 20*time.Second, 20*time.Millisecond)
		current, err = a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Nil(t, current.ScheduledChange)
		effects := p.effects(t)
		require.Len(t, effects, 2)
		require.ElementsMatch(t, []int64{1000, 2000}, []int64{effects[0].Amount, effects[1].Amount})
		cycles, err := a.alpha.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		require.Len(t, cycles.Items, 1)
		require.EqualValues(t, 20_000_000, cycles.Items[0].Amount)
	})
	t.Run("OR-J18-S02/unsupported-seats-and-retired-preview-target-refuse-without-charge", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		_, err := a.alpha.PreviewSubscriptionChange(t.Context(), sub.ID, billing.ChangeSubscriptionParams{Quantity: ptr(2)})
		require.Error(t, err)
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, billing.CodeQuantityNotAllowed, refusal.Code)
		price := upgradePrice(t, a, sub)
		params := billing.ChangeSubscriptionParams{PriceID: &price.ID, Reason: "Attempt stale target", IdempotencyKey: "stale-preview"}
		_, err = a.alpha.PreviewSubscriptionChange(t.Context(), sub.ID, params)
		require.NoError(t, err)
		_, err = a.alpha.UpdateProduct(t.Context(), price.ProductID, billing.UpdateProductParams{Archived: catalog.Value(true)})
		require.NoError(t, err)
		_, err = a.alpha.ChangeSubscription(t.Context(), sub.ID, params)
		require.Error(t, err)
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, billing.CodeSubscriptionChangeTargetInactive, refusal.Code)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, sub.PriceID, current.PriceID)
		require.Nil(t, current.ScheduledChange)
		require.Len(t, p.effects(t), 1)
	})
	t.Run("OR-J18-S03/strong-customer-action-settles-one-prorated-upgrade", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		price := upgradePrice(t, a, sub)
		a.clock.Advance(12 * time.Hour)
		path := "/v1/me/subscriptions/" + sub.ID.String()
		body := map[string]any{"price_id": price.ID}
		status, raw := a.request(t, http.MethodPost, path+"/change/preview", a.aliceToken, body)
		require.Equal(t, http.StatusOK, status, string(raw))
		var preview billing.SubscriptionChangePreview
		require.NoError(t, json.Unmarshal(raw, &preview))
		require.Equal(t, "now", preview.Effective)
		require.EqualValues(t, 15_000_000, preview.AmountDueNow)
		require.EqualValues(t, 20_000_000, preview.NextChargeAmount)
		p.core.SetDecline("4242", "auth")
		status, raw = a.request(t, http.MethodPost, path+"/change", a.aliceToken, body, map[string]string{"Idempotency-Key": "challenged-upgrade"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var waiting billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &waiting))
		require.Equal(t, "requires_action", waiting.Status)
		require.NotNil(t, waiting.NextAction)
		require.Len(t, p.effects(t), 1)
		intent, _, ok := strings.Cut(waiting.NextAction.Payload["client_secret"], "_secret_")
		require.True(t, ok)
		require.True(t, p.core.Authenticate(intent))
		status, raw = a.request(t, http.MethodPost, path+"/confirm", a.aliceToken, map[string]any{})
		require.Equal(t, http.StatusOK, status, string(raw))
		var changed billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &changed))
		require.Equal(t, "succeeded", changed.Status)
		require.Equal(t, price.ID, changed.PriceID)
		// Confirm only names a currently unresolved change. After settlement
		// there is nothing to confirm; replay the original accepted key to
		// retrieve its retained result, without another charge.
		status, raw = a.request(t, http.MethodPost, path+"/confirm", a.aliceToken, map[string]any{})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), "resource_conflict")
		status, raw = a.request(t, http.MethodPost, path+"/change", a.aliceToken, body, map[string]string{"Idempotency-Key": "challenged-upgrade"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var replay billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, changed.PriceID, replay.PriceID)
		require.Equal(t, changed.TransactionID, replay.TransactionID)
		require.NotNil(t, changed.SubscriptionID)
		require.NotEqual(t, sub.ID, *changed.SubscriptionID)
		current, err := a.alpha.GetSubscription(t.Context(), *changed.SubscriptionID)
		require.NoError(t, err)
		require.Equal(t, price.ID, current.PriceID)
		require.Equal(t, sub.CustomerID, current.CustomerID)
		require.Equal(t, billing.SubscriptionActive, current.Status)
		prior, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, prior.Status)
		effects := p.effects(t)
		require.Len(t, effects, 2)
		require.ElementsMatch(t, []int64{1000, 1500}, []int64{effects[0].Amount, effects[1].Amount})
		events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 2)
		upgraded, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: *changed.SubscriptionID})
		require.NoError(t, err)
		require.Len(t, upgraded.Items, 1)
		require.NotNil(t, upgraded.Items[0].Card, "the upgrade charge names the card it charged")
		require.Equal(t, "visa", *upgraded.Items[0].Card.Brand)
		require.Equal(t, "4242", *upgraded.Items[0].Card.Last4)

		// So does the charge for more seats.
		w, provider := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
		seats, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "team", DisplayName: "Team", Entitlements: []string{"team:member"}})
		require.NoError(t, err)
		seat, err := w.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: seats.ID, Key: "team-day", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24), Quantity: &catalog.Quantity{Min: 1, Max: 5}})
		require.NoError(t, err)
		order := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: seat.ID, Quantity: ptr(1)}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
			Payment: &billing.OrderPaymentParams{Token: provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
		status, raw = w.request(t, http.MethodPost, "/v1/me/orders", w.aliceToken, order, map[string]string{"Idempotency-Key": "team-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var bought billing.Order
		require.NoError(t, json.Unmarshal(raw, &bought))
		require.Equal(t, billing.OrderComplete, bought.Status, string(raw))
		team := *bought.Lines[0].SubscriptionID
		w.clock.Advance(12 * time.Hour)
		status, raw = w.request(t, http.MethodPost, "/v1/me/subscriptions/"+team.String()+"/change", w.aliceToken, map[string]any{"quantity": 2}, map[string]string{"Idempotency-Key": "more-seats"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var added billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &added))
		require.Equal(t, "succeeded", added.Status, string(raw))
		charged, err := w.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: team})
		require.NoError(t, err)
		require.Len(t, charged.Items, 2, "the purchase and the added seats")
		for _, payment := range charged.Items {
			require.NotNil(t, payment.Card, "each charge names the card it charged")
			require.Equal(t, "4242", *payment.Card.Last4)
		}
	})
	t.Run("OR-J18-S04/admitted-renewal-fences-concurrent-customer-upgrade", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		price := upgradePrice(t, a, sub)
		gate := p.core.Hold(func(r *http.Request) bool { return r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents" }, stripemock.HoldRequest)
		defer gate.Release()
		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 1)
		select {
		case <-gate.Arrived():
		case <-time.After(15 * time.Second):
			t.Fatal("renewal was not admitted before the competing change")
		}
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", a.aliceToken, map[string]any{"price_id": price.ID}, map[string]string{"Idempotency-Key": "competing-upgrade"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), billing.CodeSubscriptionChangeRenewalDue)
		gate.Release()
		renewalSettled(t, a, p, sub, due)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, sub.PriceID, current.PriceID)
		require.Nil(t, current.ScheduledChange)
	})
}
