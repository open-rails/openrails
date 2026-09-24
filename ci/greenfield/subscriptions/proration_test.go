//go:build greenfield && integration

package subscriptions_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// tierPrice creates a product at rank in group with one auto-renewing USD
// price of cycle hours. A day-based price may carry an NMI plan, which the
// engine creates at the provider.
type tier struct {
	*openrails.Price
	ent, plan string
}

func (w *world) tierPrice(group string, rank int, cents int64, cycle int, nmiPlan bool) tier {
	w.t.Helper()
	client := w.client[embedded]
	key := fmt.Sprintf("tier-%d-%s", rank, uuid.NewString()[:8])
	product, err := client.Products.Create(w.t.Context(), &openrails.ProductCreateParams{Key: key, DisplayName: key, TierGroup: &group, TierRank: rank,
		EntitlementsSpec: map[string]*int{"content:" + key: nil}})
	require.NoError(w.t, err)
	params := &openrails.PriceCreateParams{ProductID: product.ID, Key: key + "-usd", UnitAmount: cents * 10_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &cycle}
	out := tier{ent: "content:" + key}
	if nmiPlan {
		out.plan = "gf_plan_" + uuid.NewString()[:8]
		params.PSPLinks = map[string]map[string]string{"nmi": {"plan_id": out.plan}}
	}
	out.Price, err = client.Prices.Create(w.t.Context(), params)
	require.NoError(w.t, err)
	return out
}

// importNMI lands a provider-owned NMI membership on price (whose plan the
// engine created) with current period [start, end], via ImportBilling.
func (w *world) importNMI(price tier, cents int64, start, end time.Time) (*customer, openrails.SubscriptionID) {
	t := w.t
	t.Helper()
	c := w.newCustomer()
	vault := w.nmi.legacyVault(visa)
	amount := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	railSub := w.nmi.legacySchedule(vault, price.plan, amount, end)
	customerID, err := openrails.ParseCustomerID(c.id)
	require.NoError(t, err)
	priceID, err := openrails.ParsePriceID(price.ID)
	require.NoError(t, err)
	method := &openrails.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.billingOf(vault)}
	result, err := w.client[embedded].ImportBilling(t.Context(), openrails.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: openrails.PSPRef{Key: "nmi"},
		Customers:      []openrails.DeclaredCustomer{{Customer: customerID}},
		PaymentMethods: []openrails.DeclaredPaymentMethod{{Customer: customerID, Rail: "nmi", RailCustomerRef: vault, RailMethodRef: method.RailMethodRef, LastFour: "4242", CardType: "visa", ExpiryDate: "12/35"}},
		Subscriptions: []openrails.DeclaredSubscription{{SourceID: "import-" + railSub, Customer: customerID, Price: priceID, Rail: "nmi", RailSubscriptionID: railSub,
			StartedAt: start, PaidThrough: &end, PaymentMethod: method}},
		Transactions: []openrails.DeclaredTransaction{{RailSubscriptionID: railSub, TransactionID: w.nmi.legacySale(vault, amount, start), Success: true, AmountCents: cents, Currency: "USD", OccurredAt: start}},
	})
	require.NoError(t, err)
	require.Len(t, result.Imported, 1, "%+v", result)
	w.settle()
	subs, err := w.client[embedded].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: c.id})
	require.NoError(t, err)
	require.Len(t, subs.Data, 1)
	return c, subs.Data[0].ID
}

// requirePreview asserts both Client topologies quote charge (cents) now and
// a new period of newCycle hours.
func (w *world) requirePreview(sub openrails.SubscriptionID, target string, charge int64, newCycle int) {
	t := w.t
	t.Helper()
	for _, tp := range []topology{embedded, remote} {
		preview, err := w.client[tp].PreviewTierChange(t.Context(), sub, openrails.ChangeTierRequest{PriceID: target})
		require.NoError(t, err, tp)
		require.Equal(t, "upgrade", preview.Action)
		require.Equal(t, charge*10_000, preview.AmountDueNow, "%s preview", tp)
		require.True(t, w.clock.Now().Add(time.Duration(newCycle)*time.Hour).Equal(*preview.NextChargeDate), "new period is the new cadence")
	}
}

// Upgrades credit the old plan's unused value against its own current
// period at sub-second precision, whatever the new plan's cadence (#1067).
// Preview (embedded and remote Client) equals the durable charge, which
// equals the provider's sale exactly. NMI plans are day-based, so these
// memberships are provider-owned imports.
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
			old := w.tierPrice(group, 1, row.oldCents, row.oldCycle, true)
			next := w.tierPrice(group, 2, row.newCent, row.newCyc, true)
			end := w.clock.Now().Add(row.left)
			c, sub := w.importNMI(old, row.oldCents, end.Add(-time.Duration(row.oldCycle)*h), end)
			current := w.subscription(tp, sub)
			require.True(t, end.Equal(*current.CurrentPeriodEndsAt))
			require.Equal(t, time.Duration(row.oldCycle)*h, current.CurrentPeriodEndsAt.Sub(*current.CurrentPeriodStartsAt), "the actual current period")
			sales := len(w.nmi.ledger(""))

			w.requirePreview(sub, next.ID, row.charge, row.newCyc)
			done, err := w.client[tp].ChangeTier(t.Context(), sub, "upgrade-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: next.ID})
			require.NoError(t, err)
			w.settle()
			require.Equal(t, row.charge*10_000, done.AmountDueNow, "charged equals preview")

			ledger := w.nmi.ledger("")
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
	require.Empty(t, w.nmi.unexpected())
}

// Hourly memberships are engine-owned; their quotes carry sub-hour credit
// (the old whole-hour count credited nothing below 60 minutes).
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
			sub := c.subscribeAgain(embedded, "nmi", old.ID, old.ent, c.saveCard("nmi", visa))
			current := w.subscription(remote, sub)
			require.Equal(t, time.Hour, current.CurrentPeriodEndsAt.Sub(*current.CurrentPeriodStartsAt))
			w.advance(current.CurrentPeriodEndsAt.Sub(w.clock.Now()) - row.left)
			w.requirePreview(sub, next.ID, row.charge, 24)
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
	old := w.tierPrice(group, 1, 1000, 720, true)
	short := w.tierPrice(group, 2, 500, 168, true)
	end := w.clock.Now().Add(700 * time.Hour)
	_, sub := w.importNMI(old, 1000, end.Add(-720*time.Hour), end)
	sales := len(w.nmi.ledger(""))

	// A price without a cycle (the schema forbids one on an auto-renewing
	// price) cannot open a period: refused, never defaulted to 720h.
	noCycle := w.tierPrice(group, 3, 5000, 720, true)
	_, err := w.pool.Exec(t.Context(), `UPDATE `+pgx.Identifier{w.schema}.Sanitize()+`.prices SET auto_renew = false, access_duration_hours = NULL WHERE key = $1`, noCycle.Key)
	require.NoError(t, err)

	for _, tc := range []struct {
		target tier
		code   string
		status int
	}{
		{short, openrails.CodeTierChangeCreditExceedsPrice, 409},
		{noCycle, openrails.CodeTierChangeCycleUnknown, 422},
	} {
		for _, tp := range []topology{embedded, remote} {
			_, err := w.client[tp].PreviewTierChange(t.Context(), sub, openrails.ChangeTierRequest{PriceID: tc.target.ID})
			requireCode(t, err, tc.status, tc.code)
			_, err = w.client[tp].ChangeTier(t.Context(), sub, "refused-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: tc.target.ID})
			requireCode(t, err, tc.status, tc.code)
		}
	}
	w.settle()
	require.Len(t, w.nmi.ledger(""), sales, "a refused upgrade charges nothing")
	require.Equal(t, old.ID, w.subscription(embedded, sub).PriceID)
}

func requireCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	var statusErr *openrails.StatusError
	require.True(t, errors.As(err, &statusErr), "%v", err)
	require.Equal(t, status, statusErr.Status, "%v", err)
	require.Equal(t, code, statusErr.Code, "%v", err)
}
