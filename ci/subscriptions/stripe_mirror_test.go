//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// #1094: Stripe bills the subscriptions it owns; OpenRails mirrors them and a
// tier follows only a paid invoice for it.

// A portal upgrade whose proration invoice is still open grants nothing: the
// member keeps the paid tier. The new tier takes effect with Stripe's paid
// invoice for the new price.
func TestStripePortalUpgradeNeedsPayment(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	client := w.client[embedded]
	premium := "content:premium"
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "premium-" + uuid.NewString()[:8], DisplayName: "Premium", EntitlementsSpec: map[string]*int{l.ent: nil, premium: nil}})
	require.NoError(t, err)
	hours := monthHours
	stripePrice := "price_legacy_" + uuid.NewString()[:8]
	w.stripe.legacyPrice(stripePrice, 1999)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 19_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
		PSPLinks: map[string]map[string]string{"stripe": {"price_id": stripePrice}}})
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("customer.subscription.updated", w.stripe.portalPriceChange(l.railSub, stripePrice, 1999, 500))))
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, l.price.ID, sub.PriceID, "an open invoice pays for nothing")
	require.Equal(t, "active", sub.Status)
	require.False(t, l.c.entitled(premium))
	require.True(t, l.c.entitled(l.ent), "the paid tier stands")

	end := l.periodEnd()
	w.advance(end.Sub(w.clock.Now()) + time.Hour)
	require.Equal(t, http.StatusOK, w.deliver("stripe", l.providerRenewal(true)))
	sub = w.subscription(embedded, l.sub)
	require.Equal(t, price.ID, sub.PriceID, "Stripe's paid invoice for the new price")
	require.Equal(t, "active", sub.Status)
	require.True(t, sub.CurrentPeriodEndsAt.After(end))
	require.True(t, l.c.entitled(premium))
	require.Zero(t, l.engineCharges())
}

// A portal upgrade whose proration Stripe charged at once moves the tier and
// records that charge as a payment, without moving the paid period. The
// money invariants check the provider ledger against local payments.
func TestStripePortalUpgradePaidProrationIsRecorded(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	client := w.client[embedded]
	premium := "content:premium"
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "premium-" + uuid.NewString()[:8], DisplayName: "Premium", EntitlementsSpec: map[string]*int{l.ent: nil, premium: nil}})
	require.NoError(t, err)
	hours := monthHours
	stripePrice := "price_legacy_" + uuid.NewString()[:8]
	w.stripe.legacyPrice(stripePrice, 1999)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 19_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
		PSPLinks: map[string]map[string]string{"stripe": {"price_id": stripePrice}}})
	require.NoError(t, err)
	paidBefore := len(completed(w.payments(embedded, l.c.id)))
	end := l.periodEnd()

	require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("customer.subscription.updated", w.stripe.portalPriceChangePaid(l.railSub, stripePrice, 1999, 500))))
	w.settle()
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, price.ID, sub.PriceID, "a paid proration moves the tier")
	require.True(t, sub.CurrentPeriodEndsAt.Equal(end), "a proration never moves the paid period")
	require.True(t, l.c.entitled(premium))
	paid := completed(w.payments(embedded, l.c.id))
	require.Len(t, paid, paidBefore+1, "the proration charge is a payment")
	charge := w.stripe.latestCharge(l.railSub)
	var proration *openrails.Payment
	for i := range paid {
		if paid[i].TransactionID == charge {
			proration = &paid[i]
		}
	}
	require.NotNil(t, proration, "the proration charge %s is recorded", charge)
	require.EqualValues(t, 5_000_000, proration.Amount)

	require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("customer.subscription.updated", w.stripe.subscriptionObject(l.railSub))))
	w.settle()
	require.Len(t, completed(w.payments(embedded, l.c.id)), paidBefore+1, "a replayed event records it once")
}

// An event that lands while the converge job for its subscription is running
// is not lost: a follow-up reads Stripe again once that job finishes.
func TestStripeEventMidConvergeIsNotLost(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()

	g := newGate(func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/subscriptions/"+l.railSub)
	}, true)
	g.served = true
	w.stripe.hold(g)
	require.Equal(t, http.StatusOK, w.deliverNow("stripe", stripeEvent("customer.subscription.updated", w.stripe.subscriptionObject(l.railSub))))
	select {
	case <-g.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the converge job never read Stripe")
	}

	// The running job has read the live subscription; Stripe now ends it.
	require.Equal(t, http.StatusOK, w.deliverNow("stripe", l.providerCancelNotice()))
	close(g.release)
	w.stripe.unhold()
	w.settle()

	require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status, "the mid-run cancellation is converged")
	require.False(t, l.c.entitled(l.ent))
}
