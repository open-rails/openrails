//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

// OpenRails runs every new subscription it sells: our prices are billed from
// the merchant's card vault through its charge PSPs, and nothing is written to
// a provider's catalog. Provider prices mirror what a provider still owns and
// runs (legacy NMI plans) or sells (CCBill FlexForms), read-only.

// providerCatalogWrites are the requests that would create or edit a
// provider's catalog, or start a provider-run schedule.
func (w *world) providerCatalogWrites() []string {
	var out []string
	for _, c := range w.nmi.CallsTo(http.MethodPost, "/plans", nil) {
		out = append(out, "nmi POST "+c.Path)
	}
	for _, c := range w.nmi.CallsTo(http.MethodPost, "transact.php", func(v url.Values) bool {
		r := v.Get("recurring")
		return r == "add_plan" || r == "edit_plan" || r == "add_subscription"
	}) {
		out = append(out, "nmi recurring="+c.Form.Get("recurring"))
	}
	for _, prefix := range []string{"/v1/products", "/v1/prices", "/v1/subscriptions", "/v1/entitlements"} {
		for range w.stripe.Mutations(prefix) {
			out = append(out, "stripe POST "+prefix)
		}
	}
	return out
}

// A doujins-like merchant saves new cards in its NMI gateway's vault and
// charges them through that gateway alone. Its own monthly price, applied
// from a document, sells by card and renews from the vaulted card through the
// same gateway; no NMI plan or schedule exists for it.
func TestOurPriceSellsAndRenewsThroughTheCardVault(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	require.NoError(t, w.applySettings(t.Context(), billing.MerchantSettings{CardVault: "nmi", ChargePSPs: []string{"nmi"}}))
	key, receipt, err := w.applyCatalogWith(`  "{key}-monthly":
    amount: 23 USD
    billing_interval: 30 days
    access_duration: 30 days
`, "")
	require.NoError(t, err)
	require.Empty(t, receipt.Warnings)
	price, err := priceByKey(t.Context(), w.client[embedded], key, key+"-monthly")
	require.NoError(t, err)
	require.Nil(t, price.Provider, "the price is ours")
	options := w.options(price.ID)
	require.Contains(t, options, "nmi")
	require.NotContains(t, options, "stripe", "the charge PSPs alone take a new card")

	c := w.newCustomer()
	sub := c.subscribe(embedded, "nmi", price.ID.String(), key, c.saveCard("nmi", visa))
	got := w.subscription(embedded, sub)
	require.Equal(t, "engine", got.CollectionPolicy, "OpenRails runs the billing")
	w.advanceHealthyTo(got.CurrentPeriodEndsAt.Add(time.Minute))
	w.runRenewals()
	paid := completed(w.payments(embedded, c.id))
	require.Len(t, paid, 2, "the renewal charged the vaulted card")
	for _, p := range paid {
		require.Equal(t, w.psp["nmi"], *p.PSPID, "through the card vault's gateway")
		require.Equal(t, int64(23_000_000), p.Amount)
	}
	require.Empty(t, w.providerCatalogWrites(), "no NMI plan, schedule or Stripe catalog object")
	require.Empty(t, w.nmi.Unexpected())
}

// A catalog document declares our prices and the provider prices of a
// legacy NMI plan, a Stripe price and a CCBill FlexForm. Applying it reads
// the NMI and Stripe terms from the providers, takes CCBill's declared ones,
// and writes nothing to any provider; access lasts the billing interval
// unless declared.
func TestCatalogApplyReadsProviderPricesAndWritesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	plan := "premium_new_" + uuid.NewString()[:8]
	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Premium", Amount: "23.00", Days: 30})
	stripePrice := "price_legacy_" + uuid.NewString()[:8]
	w.stripe.SetLegacyPrice(stripePrice, 1900)
	key, receipt, err := w.applyCatalogWith(`  "{key}-monthly":
    amount: 23 USD
    billing_interval: 30 days
    access_duration: 30 days
  "{key}-90d":
    amount: 62 USD
    access_duration: 90 days
`, fmt.Sprintf(`  "{key}-nmi":
    psp: nmi
    plan_id: %s
  "{key}-stripe":
    psp: stripe
    price_id: %s
    access_duration: 60 days
  "{key}-ccbill":
    psp: ccbill
    flex_id: %s
    recurring_billing_option_id: "%s"
    amount: 23 USD
    billing_interval: 30 days
`, plan, stripePrice, ccbillFlexID, ccbillRBO))
	require.NoError(t, err)
	require.Empty(t, receipt.Warnings)
	require.Equal(t, 5, receipt.PricesChanged)

	read := func(k string) *billing.Price {
		price, err := priceByKey(t.Context(), w.client[embedded], key, key+k)
		require.NoError(t, err)
		return price
	}
	nmiPrice, stripe, ccbill := read("-nmi"), read("-stripe"), read("-ccbill")
	require.Equal(t, map[string]string{"plan_id": plan}, nmiPrice.Provider.IDs)
	require.Equal(t, []any{int64(23_000_000), "USD", monthHours, monthHours}, []any{nmiPrice.UnitAmount, nmiPrice.Currency, *nmiPrice.BillingIntervalHours, *nmiPrice.AccessDurationHours}, "NMI's terms")
	require.Equal(t, stripePrice, stripe.Provider.IDs["price_id"])
	require.Equal(t, []any{int64(19_000_000), monthHours, 60 * 24}, []any{stripe.UnitAmount, *stripe.BillingIntervalHours, *stripe.AccessDurationHours}, "Stripe's terms, declared access")
	require.Equal(t, []any{int64(23_000_000), monthHours, "ccbill"}, []any{ccbill.UnitAmount, *ccbill.BillingIntervalHours, ccbill.Provider.Rail}, "CCBill's declared terms")
	require.Nil(t, read("-monthly").Provider)

	// Legacy NMI and Stripe provider prices bill what they have; nothing new
	// sells on them.
	for _, legacy := range []*billing.Price{nmiPrice, stripe} {
		preview, err := w.client[remote].PreviewOrder(t.Context(), billing.PreviewOrderParams{CustomerID: w.newCustomer().cid(), Lines: []billing.OrderLineParams{{PriceID: legacy.ID}}})
		if err == nil {
			require.Empty(t, preview.PaymentOptions, "%s sells nothing new", legacy.Key)
		}
	}
	require.Empty(t, w.providerCatalogWrites(), "the apply wrote nothing to a provider")

	// A missing plan is refused: a provider price names an existing object.
	_, _, err = w.applyCatalogWith(``, `  "{key}-missing":
    psp: nmi
    plan_id: never_created
`)
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.Empty(t, w.providerCatalogWrites(), "never created at the provider")
}

// A provider-run subscription is imported on a provider price of its own PSP:
// one naming a price of ours, or another PSP's, is refused. On its provider
// price, NMI keeps renewing it and OpenRails records each charge; a declined
// renewal goes past due and OpenRails' dunning collects it against the
// subscription's own NMI schedule.
func TestImportedNMIScheduleBillsItsProviderPrice(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", embedded)
	require.Equal(t, "nmi", l.price.Provider.PSP)
	require.NotEmpty(t, l.price.Provider.IDs["plan_id"])

	ours := w.membership("content:ours", 9_990_000)
	c := w.newCustomer()
	vault := w.nmi.AddVault(visa)
	end := w.clock.Now().Add(20 * day)
	railSub := w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: l.price.Provider.IDs["plan_id"], Amount: "9.99", NextBilling: end})
	book := func(price billing.PriceID, psp string) billing.DeclaredBilling {
		return billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: psp}, Customers: []billing.DeclaredCustomer{{Customer: c.cid()}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: "refused-" + uuid.NewString()[:8], Customer: c.cid(), Price: price, Rail: "nmi", RailSubscriptionID: railSub,
				StartedAt: end.Add(-monthHours * time.Hour), PaidThrough: &end}}}
	}
	result, err := w.client[embedded].ImportBilling(t.Context(), book(ours.ID, "nmi"))
	require.NoError(t, err)
	require.Len(t, result.Blocked, 1, "%+v", result)
	require.Contains(t, result.Reasons[result.Blocked[0]], "provider price")

	l.w.advanceHealthyTo(l.periodEnd().Add(time.Hour))
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true)))
	w.settle()
	paid := completed(w.payments(embedded, l.c.id))
	require.Len(t, paid, 2, "NMI's renewal is recorded")
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)
	require.Equal(t, l.price.ID, w.subscription(embedded, l.sub).PriceID)

	w.nmi.SetDecline(visa.Last4, "202")
	w.advanceHealthyTo(l.periodEnd().Add(time.Hour))
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))
	w.settle()
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionPastDue, sub.Status)
	w.nmi.SetDecline(visa.Last4, "")
	w.advanceHealthyTo(nextRetry(sub).Add(time.Second))
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status, "dunning collected it")
	rebills := w.nmi.CallsTo(http.MethodPost, "transact.php", func(v url.Values) bool {
		return v.Get("recurring") == "rebill_subscription" && v.Get("subscription_id") == l.railSub
	})
	require.Len(t, rebills, 1, "against the subscription's own schedule")
	require.Empty(t, w.providerCatalogWrites())
}
