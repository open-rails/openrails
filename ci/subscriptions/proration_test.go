//go:build e2e && integration

package subscriptions_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// tierPrice creates a product at rank in group with one auto-renewing USD
// price of cycle hours. A day-based price may carry an NMI plan, which the
// engine creates at the provider.
type tier struct {
	*billing.Price
	ent, plan string
}

func (w *world) tierPrice(group string, rank int, cents int64, cycle int, nmiPlan bool) tier {
	w.t.Helper()
	client := w.client[embedded]
	key := fmt.Sprintf("tier-%d-%s", rank, uuid.NewString()[:8])
	product, err := client.CreateProduct(w.t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, TierGroup: &group, TierRank: rank,
		Entitlements: []string{"content:" + key}})
	require.NoError(w.t, err)
	params := billing.CreatePriceParams{ProductID: product.ID, Key: key + "-usd", UnitAmount: cents * 10_000, Currency: "USD", BillingIntervalHours: &cycle, AccessDurationHours: &cycle}
	out := tier{ent: "content:" + key}
	if nmiPlan {
		out.plan = "gf_plan_" + uuid.NewString()[:8]
		params.PSPLinks = map[string]map[string]string{"nmi": {"plan_id": out.plan}}
	}
	out.Price, err = client.CreatePrice(w.t.Context(), params)
	require.NoError(w.t, err)
	return out
}

// engineWithLeft enrolls an engine-owned NMI membership on price and moves
// engine time so left remains in its first period. It returns the customer,
// the membership and the vault the engine charges.
func (w *world) engineWithLeft(price tier, left time.Duration) (*customer, billing.SubscriptionID, string) {
	t := w.t
	t.Helper()
	c := w.newCustomer()
	sub := c.subscribeAgain(embedded, "nmi", price.ID.String(), price.ent, c.saveCard("nmi", visa))
	paid := completed(w.payments(embedded, c.id))
	require.Len(t, paid, 1)
	sale, _ := w.nmi.Sale(paid[0].TransactionID)
	vault := sale.Vault
	current := w.subscription(embedded, sub)
	w.advance(current.CurrentPeriodEndsAt.Sub(w.clock.Now()) - left)
	return c, sub, vault
}

// requirePreview asserts the customer's preview quotes charge (cents) now and
// a new period of newCycle hours.
func (w *world) requirePreview(c *customer, sub billing.SubscriptionID, target billing.PriceID, charge int64, newCycle int) {
	t := w.t
	t.Helper()
	preview, err := c.previewChange(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(target)})
	require.NoError(t, err)
	require.Equal(t, "now", preview.Effective)
	require.Equal(t, charge*10_000, preview.AmountDueNow, "preview")
	require.True(t, w.clock.Now().Add(time.Duration(newCycle)*time.Hour).Equal(*preview.NextChargeDate), "new period is the new cadence")
}

// Upgrades credit the old plan's unused value against its own current period
// at sub-second precision, whatever the new plan's cadence. Preview (embedded
// and remote Client) equals the durable charge, which equals the provider's
// sale exactly, on engine-owned NMI memberships.
func TestUpgradeProrationAcrossCadences(t *testing.T) {
	t.Parallel()
	const h = time.Hour
	rows := []struct {
		name              string
		oldCycle, newCyc  int
		oldCents, newCent int64
		left              time.Duration
		charge            int64 // cents
	}{
		{"12h left on 1d to 1d", 24, 24, 1000, 2000, 12 * h, 1500},
		{"7d with 84h left to 720h", 168, 720, 500, 2000, 84 * h, 1750},
		{"720h with 360h left to 7d", 720, 168, 1000, 2000, 360 * h, 1500},
		{"90d with 45d left to 365d", 90 * 24, 365 * 24, 3000, 10000, 45 * 24 * h, 8500},
		{"sub-hour: 90m left on 1d", 24, 24, 2400, 3000, 90 * time.Minute, 2850},
		{"sub-hour: 1s left on 720h", 720, 720, 1000, 2000, time.Second, 1999},
	}
	w := newWorld(t)
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			tp := []topology{embedded, remote}[i%2]
			group := "g" + uuid.NewString()[:8]
			old := w.tierPrice(group, 1, row.oldCents, row.oldCycle, false)
			next := w.tierPrice(group, 2, row.newCent, row.newCyc, false)
			c, sub, vault := w.engineWithLeft(old, row.left)
			current := w.subscription(tp, sub)
			require.Equal(t, row.left, current.CurrentPeriodEndsAt.Sub(w.clock.Now()))
			require.Equal(t, time.Duration(row.oldCycle)*h, current.CurrentPeriodEndsAt.Sub(*current.CurrentPeriodStartsAt), "the actual current period")
			sales := len(w.nmi.ledger(vault))

			w.requirePreview(c, sub, next.ID, row.charge, row.newCyc)
			done, err := c.change(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(next.ID), IdempotencyKey: "upgrade-" + uuid.NewString()})
			require.NoError(t, err)
			w.settle()
			require.Equal(t, row.charge*10_000, done.AmountDueNow, "charged equals preview")

			ledger := w.nmi.ledger(vault)
			require.Len(t, ledger, sales+1, "exactly one proration sale")
			require.Equal(t, row.charge, ledger[len(ledger)-1].Amount, "provider journal carries the quoted amount exactly")
			require.LessOrEqual(t, row.newCent-row.charge, row.oldCents, "credit never exceeds the amount paid")
			var local []int64
			for _, p := range completed(w.payments(tp, c.id)) {
				local = append(local, p.Amount)
			}
			require.Contains(t, local, row.charge*10_000, "local payment equals the provider sale")
		})
	}
	require.Empty(t, w.nmi.Unexpected())
}

// Hourly memberships are engine-owned; their quotes carry sub-hour credit.
func TestUpgradeProrationHourlyQuotes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, row := range []struct {
		name              string
		oldCents, newCent int64
		left              time.Duration
		charge            int64
	}{
		{"30m left on 1h", 200, 2000, 30 * time.Minute, 1900},
		{"59m59s left on 1h", 3600, 5000, 59*time.Minute + 59*time.Second, 1401},
	} {
		t.Run(row.name, func(t *testing.T) {
			group := "g" + uuid.NewString()[:8]
			old := w.tierPrice(group, 1, row.oldCents, 1, false)
			next := w.tierPrice(group, 2, row.newCent, 24, true)
			c := w.newCustomer()
			sub := c.subscribeAgain(embedded, "nmi", old.ID.String(), old.ent, c.saveCard("nmi", visa))
			current := w.subscription(remote, sub)
			require.Equal(t, time.Hour, current.CurrentPeriodEndsAt.Sub(*current.CurrentPeriodStartsAt))
			w.advance(current.CurrentPeriodEndsAt.Sub(w.clock.Now()) - row.left)
			w.requirePreview(c, sub, next.ID, row.charge, 24)
		})
	}
}

// Refusals are typed on both topologies and charge nothing: a long-cadence
// plan early in its period is worth more than a short-cadence target, and a
// target without a positive cycle cannot open a period.
func TestUpgradeProrationRefusals(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	old := w.tierPrice(group, 1, 1000, 720, false)
	short := w.tierPrice(group, 2, 500, 168, false)
	c, sub, vault := w.engineWithLeft(old, 700*time.Hour)
	sales := len(w.nmi.ledger(vault))

	// A price without a cycle (the schema forbids one on an auto-renewing
	// price) cannot open a period: refused, never defaulted to 720h.
	noCycle := w.tierPrice(group, 3, 5000, 720, false)
	oneOff, err := w.client[embedded].CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: noCycle.ProductID, Key: noCycle.Key, UnitAmount: noCycle.UnitAmount, Currency: noCycle.Currency})
	require.NoError(t, err)
	noCycle.Price = oneOff

	for _, tc := range []struct {
		target tier
		code   string
		status int
	}{
		{short, billing.CodeSubscriptionChangeCreditExceedsPrice, 409},
		{noCycle, billing.CodeSubscriptionChangeCycleUnknown, 422},
	} {
		_, err := c.previewChange(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(tc.target.ID)})
		requireCode(t, err, tc.status, tc.code)
		_, err = c.change(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(tc.target.ID), IdempotencyKey: "refused-" + uuid.NewString()})
		requireCode(t, err, tc.status, tc.code)
	}
	w.settle()
	require.Len(t, w.nmi.ledger(vault), sales, "a refused upgrade charges nothing")
	require.Equal(t, old.ID, w.subscription(embedded, sub).PriceID)
}

func requireCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	var statusErr *billing.StatusError
	require.True(t, errors.As(err, &statusErr), "%v", err)
	require.Equal(t, status, statusErr.Status, "%v", err)
	require.Equal(t, code, statusErr.Code, "%v", err)
}
