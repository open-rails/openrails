//go:build integration

package critical_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
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

// stripeSeats is a provider-owned Stripe subscription billing three seats of
// Gold, ranked above Silver. Stripe's item quantity is the only record of
// its seats.
type stripeSeats struct {
	*authorityWorld
	stripe       *stripemock.Mock
	sub          *billing.Subscription
	gold, silver *billing.Price
	start, end   time.Time
}

func stripeSeatMembership(t *testing.T) *stripeSeats {
	t.Helper()
	var accounts *stripeAccounts
	a := newAuthorityWorld(t, func(w *world) {
		accounts = newStripeAccounts(t, w, "sk_test_seats")
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		m := w.cfg.Merchants["alpha"]
		m.PSPs = openrails.PSPs{"seats": stripePSP("acct_seats", "sk_test_seats", false)}
		w.cfg.Merchants["alpha"] = m
	})
	s := &stripeSeats{authorityWorld: a, stripe: accounts.accounts["sk_test_seats"].core}
	prices := map[string]*billing.Price{}
	for rank, tier := range []string{"silver", "gold"} {
		s.stripe.AddProduct("prod_"+tier, tier)
		s.stripe.AddPrice(stripemock.Price{ID: "price_" + tier, Product: "prod_" + tier, Amount: int64(1000 * (rank + 1)), Interval: "month"})
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: tier, DisplayName: tier, TierGroup: ptr("seat-membership"), TierRank: rank + 1, Entitlements: []string{"member:" + tier}})
		require.NoError(t, err)
		prices[tier], err = a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "stripe-" + tier, PSP: "seats", PriceID: "price_" + tier})
		require.NoError(t, err)
	}
	s.gold, s.silver = prices["gold"], prices["silver"]
	s.start = a.clock.Now().Truncate(time.Second)
	s.end = s.start.Add(30 * 24 * time.Hour)
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	remote := s.stripe.AddSubscription(s.stripe.AddCustomer(a.alice.Email, "Seat member", nil), "pm_seats", "price_gold", 6000, s.start, s.end)
	s.stripe.SetQuantity(remote, 3)
	book := billing.DeclaredBilling{AsOf: s.start, DefaultPSP: billing.PSPRef{Key: "seats"}, Customers: []billing.DeclaredCustomer{{Customer: customer}},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "three-seat-stripe-membership", Customer: customer, Price: s.gold.ID, Rail: "stripe", RailSubscriptionID: remote, StartedAt: s.start, PaidThrough: &s.end}}}
	imported, err := a.alpha.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
	subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	s.sub, err = a.alpha.GetSubscription(t.Context(), subs.Items[0].ID)
	require.NoError(t, err)
	require.Equal(t, billing.SubscriptionActive, s.sub.Status)
	return s
}

// phasesForm is the exact schedule a change at period end writes: Gold's
// three seats until the period ends, then next's three seats, released.
func (s *stripeSeats) phasesForm(next string, extra url.Values) url.Values {
	form := url.Values{
		"end_behavior": {"release"}, "proration_behavior": {"none"},
		"phases[0][items][0][price]": {"price_gold"}, "phases[0][items][0][quantity]": {"3"},
		"phases[0][start_date]": {strconv.FormatInt(s.start.Unix(), 10)}, "phases[0][end_date]": {strconv.FormatInt(s.end.Unix(), 10)},
		"phases[1][items][0][price]": {next}, "phases[1][items][0][quantity]": {"3"},
		"phases[1][duration][interval]": {"month"}, "phases[1][duration][interval_count]": {"1"},
	}
	maps.Copy(form, extra)
	return form
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
	t.Run("OR-J18-S06/stripe-downgrade-keeps-the-subscriptions-seats", func(t *testing.T) {
		s := stripeSeatMembership(t)
		changed, err := s.alpha.ChangeSubscription(t.Context(), s.sub.ID, billing.ChangeSubscriptionParams{PriceID: &s.silver.ID, Reason: "Customer asked for the smaller plan", IdempotencyKey: "three-seat-downgrade"})
		require.NoError(t, err)
		require.Equal(t, "succeeded", changed.Status, changed.Message)
		require.Equal(t, "period_end", changed.Effective)
		remote := *s.sub.RailSubscriptionID
		creates := s.stripe.Submitted("/v1/subscription_schedules")
		require.Len(t, creates, 1)
		key, ok := strings.CutSuffix(creates[0].IdempotencyKey, ":schedule")
		require.True(t, ok, creates[0].IdempotencyKey)
		require.Equal(t, url.Values{"from_subscription": {remote}}, creates[0].Form)
		schedule, _ := s.stripe.Subscription(remote)["schedule"].(string)
		require.NotEmpty(t, schedule)
		writes := s.stripe.Submitted("/v1/subscription_schedules/" + schedule)
		require.Len(t, writes, 1)
		require.Equal(t, key+":phases", writes[0].IdempotencyKey)
		require.Equal(t, s.phasesForm("price_silver", url.Values{"metadata[openrails_tier_change]": {key}}), writes[0].Form, "Silver bills the same three seats from the switch")
		current, err := s.alpha.GetSubscription(t.Context(), s.sub.ID)
		require.NoError(t, err)
		require.Equal(t, s.gold.ID, current.PriceID)
		require.NotNil(t, current.ScheduledChange)
		require.Equal(t, s.silver.ID, current.ScheduledChange.PriceID)
	})
}
