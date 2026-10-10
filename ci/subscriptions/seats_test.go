//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// Seats (#1168): a recurring price is per seat only when the catalog gives
// its bounds. More seats charge the added ones for the rest of the period,
// fewer apply at the next renewal, a tier change keeps them, and a change by
// staff never charges and waits for the renewal. Renewals, cycles, payments,
// access and entitlement checks carry the seats.

// seatPrice is a per-seat tier price of cents per seat for a 30-day cycle.
func (w *world) seatPrice(group string, rank int, cents int64, bounds catalog.Quantity) tier {
	w.t.Helper()
	client := w.client[embedded]
	key := fmt.Sprintf("seats-%d-%s", rank, uuid.NewString()[:8])
	product, err := client.CreateProduct(w.t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, TierGroup: &group, TierRank: rank,
		Entitlements: []string{"content:" + key}})
	require.NoError(w.t, err)
	cycle := monthHours
	price, err := client.CreatePrice(w.t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key + "-usd", UnitAmount: cents * 10_000, Currency: "USD",
		BillingIntervalHours: &cycle, AccessDurationHours: &cycle, Quantity: &bounds})
	require.NoError(w.t, err)
	require.Equal(w.t, &bounds, price.Quantity)
	return tier{Price: price, ent: "content:" + key}
}

// seatsHeld is the seats the customer holds of an entitlement and of a
// product, as the merchant's reads answer: the entitlement's row, and the
// most a live window of the product gives.
func (w *world) seatsHeld(c *customer, entitlement string, product billing.ProductID) (*int, *int) {
	w.t.Helper()
	held, err := w.client[remote].ListEntitlements(w.t.Context(), billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{c.cid()}, Entitlements: []string{entitlement}})
	require.NoError(w.t, err)
	require.Len(w.t, held.Items, 1)
	windows, err := w.client[embedded].ListProductAccess(w.t.Context(), billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{c.cid()}, ProductIDs: []billing.ProductID{product}, LiveOnly: true})
	require.NoError(w.t, err)
	require.NotEmpty(w.t, windows.Items)
	var productSeats *int
	for _, window := range windows.Items {
		if window.Quantity != nil && (productSeats == nil || *window.Quantity > *productSeats) {
			productSeats = window.Quantity
		}
	}
	return held.Items[0].Quantity, productSeats
}

func (w *world) latestCycleSeats(sub billing.SubscriptionID) *int {
	w.t.Helper()
	var seats *int
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT quantity FROM billing.rebill_cycles WHERE subscription_id = $1 ORDER BY due_at DESC LIMIT 1`), sub.UUID()).Scan(&seats))
	return seats
}

func paymentSeats(payments []billing.Payment, amountCents int64) *int {
	for _, p := range payments {
		if p.Amount == amountCents*10_000 {
			return p.Quantity
		}
	}
	return nil
}

func TestSeats(t *testing.T) {
	t.Parallel()
	forEachRail(t, func(t *testing.T, rail string) {
		w := newWorld(t)
		group := "g" + uuid.NewString()[:8]
		team := w.seatPrice(group, 1, 1000, catalog.Quantity{Min: 1, Max: 10})
		business := w.seatPrice(group, 2, 2000, catalog.Quantity{Min: 1, Max: 10})
		c, sub := w.engineMember(rail, embedded, team)
		current := w.subscription(embedded, sub)
		require.Equal(t, seats(1), current.Quantity, "a session buys one seat")
		end := *current.CurrentPeriodEndsAt

		// More seats: the added seats for the rest of the period, now.
		w.advance(end.Sub(w.clock.Now()) - 360*time.Hour)
		charges := len(w.railLedger(rail))
		preview, err := c.previewChange(sub, billing.ChangeSubscriptionParams{Quantity: seats(3)})
		require.NoError(t, err)
		require.Equal(t, "now", preview.Effective)
		require.Equal(t, int64(1000*10_000), preview.AmountDueNow, "2 added seats for half the period")
		require.Equal(t, int64(3000*10_000), preview.NextChargeAmount)
		key := "seats-" + uuid.NewString()
		done, err := c.change(sub, billing.ChangeSubscriptionParams{Quantity: seats(3), IdempotencyKey: key})
		require.NoError(t, err)
		require.Equal(t, "succeeded", done.Status, "%+v", done)
		require.Equal(t, sub, *done.SubscriptionID, "seats change the subscription in place")
		require.Equal(t, seats(3), done.Quantity)
		ledger := w.railLedger(rail)
		require.Len(t, ledger, charges+1)
		require.Equal(t, int64(1000), ledger[len(ledger)-1].Amount)
		again, err := c.change(sub, billing.ChangeSubscriptionParams{Quantity: seats(3), IdempotencyKey: key})
		require.NoError(t, err)
		require.Equal(t, "succeeded", again.Status)
		require.Len(t, w.railLedger(rail), charges+1, "a replay never charges twice")
		current = w.subscription(remote, sub)
		require.Equal(t, seats(3), current.Quantity)
		require.True(t, end.Equal(*current.CurrentPeriodEndsAt), "the period stays")
		require.Equal(t, seats(2), paymentSeats(w.payments(embedded, c.id), 1000), "the payment records the seats it added")
		entitlementSeats, productSeats := w.seatsHeld(c, team.ent, team.ProductID)
		require.Equal(t, seats(3), entitlementSeats)
		require.Equal(t, seats(3), productSeats)

		// Fewer seats wait for the renewal; nothing is credited.
		decrease, err := c.change(sub, billing.ChangeSubscriptionParams{Quantity: seats(2), IdempotencyKey: "fewer-" + uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, "period_end", decrease.Effective)
		require.Zero(t, decrease.AmountDueNow)
		require.Equal(t, int64(2000*10_000), decrease.NextChargeAmount)
		current = w.subscription(embedded, sub)
		require.Equal(t, seats(3), current.Quantity)
		require.NotNil(t, current.ScheduledChange)
		require.Equal(t, seats(2), current.ScheduledChange.Quantity)
		require.Len(t, w.railLedger(rail), charges+1)

		w.advanceHealthyTo(end.Add(time.Second))
		w.runRenewals()
		ledger = w.railLedger(rail)
		require.Len(t, ledger, charges+2)
		require.Equal(t, int64(2000), ledger[len(ledger)-1].Amount, "the renewal bills two seats")
		current = w.subscription(remote, sub)
		require.Equal(t, seats(2), current.Quantity)
		require.Nil(t, current.ScheduledChange)
		require.Equal(t, seats(2), w.latestCycleSeats(sub), "the cycle records the seats it billed")
		require.Equal(t, seats(2), paymentSeats(w.payments(embedded, c.id), 2000))
		entitlementSeats, _ = w.seatsHeld(c, team.ent, team.ProductID)
		require.Equal(t, seats(2), entitlementSeats)

		// A tier change keeps the seats: new price × seats against old.
		renewedEnd := *current.CurrentPeriodEndsAt
		w.advance(renewedEnd.Sub(w.clock.Now()) - 360*time.Hour)
		upgrade, err := c.change(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(business.ID), IdempotencyKey: "up-" + uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, "succeeded", upgrade.Status, "%+v", upgrade)
		require.Equal(t, int64(3000*10_000), upgrade.AmountDueNow, "4000 for two seats less half of 2000")
		require.Equal(t, int64(4000*10_000), upgrade.NextChargeAmount)
		successor := *upgrade.SubscriptionID
		next := w.subscription(embedded, successor)
		require.Equal(t, seats(2), next.Quantity)
		require.Equal(t, business.ID, next.PriceID)

		// Staff add seats now at the customer's request, merchant-initiated
		// under the card's agreement; fewer seats wait for the renewal.
		w.advance(next.CurrentPeriodEndsAt.Sub(w.clock.Now()) - 360*time.Hour)
		charges = len(w.railLedger(rail))
		staffPreview, err := w.client[remote].PreviewSubscriptionChange(t.Context(), successor, billing.ChangeSubscriptionParams{Quantity: seats(5)})
		require.NoError(t, err)
		require.Equal(t, "now", staffPreview.Effective)
		require.Equal(t, int64(3000*10_000), staffPreview.AmountDueNow, "3 added seats of 2000 for half the period")
		staff, err := w.client[embedded].ChangeSubscription(t.Context(), successor, billing.ChangeSubscriptionParams{Reason: "five seats by phone", Quantity: seats(5), IdempotencyKey: "staff-" + uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, "succeeded", staff.Status, "%+v", staff)
		require.Equal(t, int64(3000*10_000), staff.AmountDueNow)
		require.Equal(t, int64(10000*10_000), staff.NextChargeAmount)
		ledger = w.railLedger(rail)
		require.Len(t, ledger, charges+1)
		require.Equal(t, int64(3000), ledger[len(ledger)-1].Amount)
		w.requireMerchantInitiated(rail, ledger[len(ledger)-1])
		w.requireStaffCharge(c, successor, 3000, "five seats by phone")
		require.Equal(t, seats(5), w.subscription(remote, successor).Quantity)
		fewer, err := w.client[remote].ChangeSubscription(t.Context(), successor, billing.ChangeSubscriptionParams{Reason: "four seats by email", Quantity: seats(4), IdempotencyKey: "staff-" + uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, "period_end", fewer.Effective)
		require.Zero(t, fewer.AmountDueNow)
		require.Equal(t, int64(8000*10_000), fewer.NextChargeAmount)
		require.Equal(t, seats(4), w.subscription(remote, successor).ScheduledChange.Quantity)
		w.requireStaffSchedule(c, successor, "four seats by email")
		w.advanceHealthyTo(next.CurrentPeriodEndsAt.Add(time.Second))
		w.runRenewals()
		ledger = w.railLedger(rail)
		require.Len(t, ledger, charges+2)
		require.Equal(t, int64(8000), ledger[len(ledger)-1].Amount, "the renewal bills the staff's seats")
		require.Equal(t, seats(4), w.subscription(embedded, successor).Quantity)

		// Another customer cannot change this subscription.
		other := w.newCustomer()
		_, err = other.change(successor, billing.ChangeSubscriptionParams{Quantity: seats(9), IdempotencyKey: "foreign-" + uuid.NewString()})
		requireCode(t, err, http.StatusNotFound, "subscription_not_found")
		_, err = c.change(successor, billing.ChangeSubscriptionParams{Quantity: seats(11), IdempotencyKey: "bounds-" + uuid.NewString()})
		requireCode(t, err, http.StatusBadRequest, billing.CodeInvalidParam)
		require.Empty(t, w.stripe.Unexpected())
		require.Empty(t, w.nmi.Unexpected())
	})
}

// A price without bounds has no seats: none on the subscription, its access
// or its checks, and a request naming some is refused. A provider-owned
// subscription never takes seats.
func TestSeatsRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	flat := w.tierPrice(group, 1, 1000, monthHours, false)
	c, sub := w.engineMember("nmi", embedded, flat)
	require.Nil(t, w.subscription(embedded, sub).Quantity)
	entitlementSeats, productSeats := w.seatsHeld(c, flat.ent, flat.ProductID)
	require.Nil(t, entitlementSeats)
	require.Nil(t, productSeats)
	_, err := c.previewChange(sub, billing.ChangeSubscriptionParams{Quantity: seats(2)})
	requireCode(t, err, http.StatusUnprocessableEntity, billing.CodeQuantityNotAllowed)
	_, err = w.client[remote].ChangeSubscription(t.Context(), sub, billing.ChangeSubscriptionParams{Reason: "customer asked", Quantity: seats(2), IdempotencyKey: "staff-" + uuid.NewString()})
	requireCode(t, err, http.StatusUnprocessableEntity, billing.CodeQuantityNotAllowed)

	legacyGroup := "g" + uuid.NewString()[:8]
	old := w.tierPrice(legacyGroup, 1, 999, monthHours, true)
	perSeat := w.seatPrice(legacyGroup, 2, 1999, catalog.Quantity{Min: 1, Max: 5})
	l := w.legacyOnTier(embedded, old, 999, monthHours, 10*day)
	_, err = l.c.change(l.sub, billing.ChangeSubscriptionParams{PriceID: priceRef(perSeat.ID), Quantity: seats(2), IdempotencyKey: "provider-" + uuid.NewString()})
	requireCode(t, err, http.StatusBadRequest, billing.CodeSubscriptionChangeUnsupportedOnRail)

	// The catalog sells seats only where OpenRails bills them.
	cycle := monthHours
	for _, params := range []billing.CreatePriceParams{
		{ProductID: perSeat.ProductID, Key: "linked", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: &cycle, Quantity: &catalog.Quantity{Min: 1, Max: 5},
			PSPLinks: map[string]map[string]string{"nmi": {"plan_id": "seat_plan"}}},
		{ProductID: perSeat.ProductID, Key: "one-off", UnitAmount: 10_000_000, Currency: "USD", Quantity: &catalog.Quantity{Min: 1, Max: 5}},
		{ProductID: perSeat.ProductID, Key: "bounds", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: &cycle, Quantity: &catalog.Quantity{Min: 3, Max: 2}},
	} {
		_, err := w.client[embedded].CreatePrice(t.Context(), params)
		require.ErrorIs(t, err, billing.ErrInvalid, params.Key)
	}
}
