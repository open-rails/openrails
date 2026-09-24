//go:build greenfield && integration

package subscriptions_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// Engine-owned tier changes (#1071). An upgrade is one engine charge of new
// price − unused credit on the membership's card, effective now: a successor
// membership opens a period of the new cadence and replaces the old one. A
// downgrade takes effect at period end: nothing is charged or refunded now,
// and the renewal bills the new price for a period of its cadence.

func (w *world) engineMember(rail string, tp topology, from tier) (*customer, openrails.SubscriptionID) {
	w.t.Helper()
	c := w.newCustomer()
	sub := c.subscribeAgain(tp, rail, from.ID, from.ent, c.saveCard(rail, visa))
	require.Equal(w.t, "engine", w.subscription(tp, sub).CollectionPolicy)
	return c, sub
}

func (w *world) railLedger(rail string) []ledgerEntry {
	if rail == "stripe" {
		return w.stripe.ledger("")
	}
	return w.nmi.ledger("")
}

func (w *world) railDecline(rail, stripeCode, nmiCode string) {
	if rail == "stripe" {
		w.stripe.setDecline(visa.Last4, stripeCode)
	} else {
		w.nmi.setDecline(visa.Last4, nmiCode)
	}
}

// forEachRail runs one world per rail; rows alternate the topology that
// makes the change, and the other topology reads and replays it.
func forEachRail(t *testing.T, run func(t *testing.T, rail string)) {
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			run(t, rail)
		})
	}
}

func other(tp topology) topology {
	if tp == embedded {
		return remote
	}
	return embedded
}

func TestEngineTierUpgrade(t *testing.T) {
	t.Parallel()
	const h = time.Hour
	rows := []struct {
		name               string
		oldCycle, newCycle int
		oldCents, newCents int64
		left               time.Duration
		charge             int64 // cents
	}{
		{"720h to 720h", 720, 720, 1000, 2000, 360 * h, 1500},
		{"1h to 720h", 1, 720, 200, 2000, 30 * time.Minute, 1900},
		{"720h to 7d", 720, 168, 1000, 2000, 360 * h, 1500},
		{"7d to 720h", 168, 720, 500, 2000, 84 * h, 1750},
	}
	forEachRail(t, func(t *testing.T, rail string) {
		w := newWorld(t)
		for i, row := range rows {
			tp := []topology{embedded, remote}[i%2]
			group := "g" + uuid.NewString()[:8]
			from := w.tierPrice(group, 1, row.oldCents, row.oldCycle, false)
			to := w.tierPrice(group, 2, row.newCents, row.newCycle, false)
			req := openrails.ChangeTierRequest{PriceID: to.ID}
			c, sub := w.engineMember(rail, tp, from)
			w.advance(w.subscription(tp, sub).CurrentPeriodEndsAt.Sub(w.clock.Now()) - row.left)
			charges := len(w.railLedger(rail))

			w.requirePreview(sub, to.ID, row.charge, row.newCycle)
			key := "upgrade-" + uuid.NewString()
			done, err := w.client[tp].ChangeTier(t.Context(), sub, key, req)
			require.NoError(t, err, row.name)
			require.Equal(t, "succeeded", done.Status, "%s: %+v", row.name, done)
			require.Equal(t, "upgrade", done.Action)
			require.Equal(t, "now", done.Effective)
			require.Equal(t, row.charge*10_000, done.AmountDueNow, "%s: charged equals preview", row.name)
			require.Equal(t, row.newCents*10_000, done.NextChargeAmount)
			ledger := w.railLedger(rail)
			require.Len(t, ledger, charges+1, "%s: one prorated engine charge", row.name)
			require.Equal(t, row.charge, ledger[len(ledger)-1].Amount, "%s: the provider journal carries the quote", row.name)

			successor := *done.SubscriptionID
			require.NotEqual(t, sub, successor)
			require.Equal(t, "cancelled", w.subscription(tp, sub).Status, "the old membership is superseded")
			next := w.subscription(other(tp), successor)
			require.Equal(t, "active", next.Status)
			require.Equal(t, to.ID, next.PriceID)
			require.Equal(t, "engine", next.CollectionPolicy)
			require.True(t, w.clock.Now().Add(time.Duration(row.newCycle)*h).Equal(*next.CurrentPeriodEndsAt), "%s: a fresh period of the new cadence", row.name)
			require.True(t, c.entitled(to.ent))
			require.False(t, c.entitled(from.ent))

			again, err := w.client[other(tp)].ChangeTier(t.Context(), sub, key, req)
			require.NoError(t, err)
			require.Equal(t, "succeeded", again.Status)
			require.Equal(t, successor, *again.SubscriptionID)
			require.Len(t, w.railLedger(rail), charges+1, "a retried change is never charged twice")

			end := *next.CurrentPeriodEndsAt
			w.advance(end.Sub(w.clock.Now()) + time.Second)
			w.runRenewals()
			ledger = w.railLedger(rail)
			require.Len(t, ledger, charges+2, "%s: one renewal", row.name)
			require.Equal(t, row.newCents, ledger[len(ledger)-1].Amount, "%s: the renewal bills the new price", row.name)
			renewed := w.subscription(tp, successor)
			require.True(t, end.Add(time.Duration(row.newCycle)*h).Equal(*renewed.CurrentPeriodEndsAt), "%s: renewal extends by the new cadence", row.name)
			require.True(t, c.entitled(to.ent))

			c.must(http.MethodPost, "/subscriptions/"+successor.String()+"/cancel", "", map[string]any{"feedback": "done"})
			w.settle()
		}
		require.Empty(t, w.stripe.unexpected())
		require.Empty(t, w.nmi.unexpected())
	})
}

func TestEngineTierDowngrade(t *testing.T) {
	t.Parallel()
	const h = time.Hour
	rows := []struct {
		name               string
		oldCycle, newCycle int
	}{
		{"720h to 720h", 720, 720},
		{"720h to 7d", 720, 168},
		{"7d to 720h", 168, 720},
	}
	forEachRail(t, func(t *testing.T, rail string) {
		w := newWorld(t)
		for i, row := range rows {
			tp := []topology{embedded, remote}[i%2]
			group := "g" + uuid.NewString()[:8]
			lowest := w.tierPrice(group, 1, 500, row.newCycle, false)
			low := w.tierPrice(group, 2, 1000, row.newCycle, false)
			high := w.tierPrice(group, 3, 2000, row.oldCycle, false)
			top := w.tierPrice(group, 4, 3000, row.oldCycle, false)
			req := openrails.ChangeTierRequest{PriceID: low.ID}
			c, sub := w.engineMember(rail, tp, high)
			end := *w.subscription(tp, sub).CurrentPeriodEndsAt
			w.advance(time.Hour)
			charges := len(w.railLedger(rail))

			for _, p := range []topology{embedded, remote} {
				preview, err := w.client[p].PreviewTierChange(t.Context(), sub, req)
				require.NoError(t, err)
				require.Equal(t, "downgrade", preview.Action)
				require.Equal(t, "period_end", preview.Effective)
				require.Zero(t, preview.AmountDueNow)
				require.Equal(t, int64(1000*10_000), preview.NextChargeAmount)
				require.True(t, end.Equal(*preview.NextChargeDate), "%s: takes effect at period end", row.name)
			}
			key := "downgrade-" + uuid.NewString()
			done, err := w.client[tp].ChangeTier(t.Context(), sub, key, req)
			require.NoError(t, err)
			require.Equal(t, "succeeded", done.Status, "%+v", done)
			require.Equal(t, "period_end", done.Effective)
			require.Zero(t, done.AmountDueNow)
			require.True(t, end.Equal(*done.DelayedStart))
			again, err := w.client[other(tp)].ChangeTier(t.Context(), sub, key, req)
			require.NoError(t, err)
			require.Equal(t, "succeeded", again.Status, "the same downgrade replays")
			_, err = w.client[tp].ChangeTier(t.Context(), sub, "downgrade-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: lowest.ID})
			requireCode(t, err, http.StatusConflict, openrails.CodeTierChangeAlreadyScheduled)

			current := w.subscription(tp, sub)
			require.Equal(t, high.ID, current.PriceID, "the current plan stays until period end")
			require.Equal(t, "active", current.Status)
			require.True(t, c.entitled(high.ent))
			require.Len(t, w.railLedger(rail), charges, "nothing is charged or refunded now")

			w.advance(end.Sub(w.clock.Now()) + time.Second)
			_, err = w.client[tp].PreviewTierChange(t.Context(), sub, openrails.ChangeTierRequest{PriceID: top.ID})
			requireCode(t, err, http.StatusConflict, openrails.CodeTierChangeRenewalDue)
			w.runRenewals()
			ledger := w.railLedger(rail)
			require.Len(t, ledger, charges+1)
			require.Equal(t, int64(1000), ledger[len(ledger)-1].Amount, "%s: the renewal bills the new price", row.name)
			renewed := w.subscription(other(tp), sub)
			require.Equal(t, low.ID, renewed.PriceID)
			require.Equal(t, "active", renewed.Status)
			require.True(t, end.Add(time.Duration(row.newCycle)*h).Equal(*renewed.CurrentPeriodEndsAt), "%s: a period of the new cadence", row.name)
			require.True(t, c.entitled(low.ent))
			require.False(t, c.entitled(high.ent))

			c.must(http.MethodPost, "/subscriptions/"+sub.String()+"/cancel", "", map[string]any{"feedback": "done"})
			w.settle()
		}
		require.Empty(t, w.stripe.unexpected())
		require.Empty(t, w.nmi.unexpected())
	})
}

// A declined upgrade charge changes nothing: the member keeps the old plan,
// price, period and access, and a later upgrade is admitted normally.
func TestEngineTierUpgradeDecline(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			group := "g" + uuid.NewString()[:8]
			from := w.tierPrice(group, 1, 1000, 720, false)
			to := w.tierPrice(group, 2, 2000, 720, false)
			req := openrails.ChangeTierRequest{PriceID: to.ID}
			c, sub := w.engineMember(rail, embedded, from)
			end := *w.subscription(embedded, sub).CurrentPeriodEndsAt
			w.advance(360 * time.Hour)
			charges := len(w.railLedger(rail))

			w.railDecline(rail, "insufficient_funds", "202")
			key := "declined-" + uuid.NewString()
			var refusal error
			w.until(func() bool {
				_, refusal = w.client[remote].ChangeTier(t.Context(), sub, key, req)
				return refusal != nil
			}, "the declined upgrade resolves as a refusal")
			var status *openrails.StatusError
			require.True(t, errors.As(refusal, &status), "%v", refusal)
			require.Equal(t, http.StatusPaymentRequired, status.Status, "%v", refusal)
			require.NotEmpty(t, status.Code)

			current := w.subscription(embedded, sub)
			require.Equal(t, "active", current.Status)
			require.Equal(t, from.ID, current.PriceID)
			require.True(t, end.Equal(*current.CurrentPeriodEndsAt))
			require.True(t, c.entitled(from.ent))
			require.False(t, c.entitled(to.ent))
			require.Len(t, w.railLedger(rail), charges)
			subs, err := w.client[embedded].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: c.id})
			require.NoError(t, err)
			require.Len(t, subs.Data, 1, "no successor membership")

			w.railDecline(rail, "", "")
			done, err := w.client[embedded].ChangeTier(t.Context(), sub, "retry-"+uuid.NewString(), req)
			require.NoError(t, err)
			require.Equal(t, "succeeded", done.Status, "%+v", done)
			require.Len(t, w.railLedger(rail), charges+1)
			require.True(t, c.entitled(to.ent))
		})
	}
}

// An upgrade the issuer challenges waits for the member's authentication of
// that same payment, then completes; nothing changes before.
func TestEngineTierUpgradeAuthentication(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	from := w.tierPrice(group, 1, 1000, 720, false)
	to := w.tierPrice(group, 2, 2000, 720, false)
	c, sub := w.engineMember("stripe", embedded, from)
	w.advance(360 * time.Hour)
	charges := len(w.stripe.ledger(""))

	w.stripe.setDecline(visa.Last4, "auth")
	key := "challenged-" + uuid.NewString()
	req := openrails.ChangeTierRequest{PriceID: to.ID}
	pending, err := w.client[embedded].ChangeTier(t.Context(), sub, key, req)
	require.NoError(t, err)
	require.Equal(t, "requires_action", pending.Status, "%+v", pending)
	require.Equal(t, "payment_authentication", pending.NextAction.Type)
	op := pending.OperationID
	require.Equal(t, from.ID, w.subscription(embedded, sub).PriceID)
	require.False(t, c.entitled(to.ent))
	require.Len(t, w.stripe.ledger(""), charges)

	auth := unwrap(c.must(http.MethodGet, "/payment-operations/"+op+"/authentication", "", nil))
	require.NotEmpty(t, auth["client_secret"])
	require.True(t, w.stripe.authenticate(auth["payment_intent_id"].(string)))
	c.must(http.MethodPost, "/payment-operations/"+op+"/authentication/confirm", "", nil)
	w.settle()

	done, err := w.client[remote].ChangeTier(t.Context(), sub, key, req)
	require.NoError(t, err)
	require.Equal(t, "succeeded", done.Status, "%+v", done)
	require.Equal(t, int64(1500*10_000), done.AmountDueNow)
	ledger := w.stripe.ledger("")
	require.Len(t, ledger, charges+1)
	require.Equal(t, int64(1500), ledger[len(ledger)-1].Amount)
	require.Equal(t, "cancelled", w.subscription(embedded, sub).Status)
	require.Equal(t, to.ID, w.subscription(embedded, *done.SubscriptionID).PriceID)
	require.True(t, c.entitled(to.ent))
	require.False(t, c.entitled(from.ent))
}
