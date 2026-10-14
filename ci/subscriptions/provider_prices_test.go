//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
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
	w := prepareWorld(t, 12)
	w.settings = billing.MerchantSettings{CardVault: "nmi"}
	w.start()
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
	require.Equal(t, []any{int64(23_000_000), "USD", monthHours}, []any{nmiPrice.UnitAmount, nmiPrice.Currency, *nmiPrice.BillingIntervalHours}, "NMI's fixed terms")
	require.Nil(t, nmiPrice.AccessDurationHours, "default follows the verified provider period")
	require.True(t, nmiPrice.Provider.AccessDurationDefault)
	require.Equal(t, stripePrice, stripe.Provider.IDs["price_id"])
	require.Equal(t, []any{int64(19_000_000), 1, 60 * 24}, []any{stripe.UnitAmount, *stripe.BillingIntervalMonths, *stripe.AccessDurationHours}, "Stripe's terms, declared access")
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
	l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
	w.converge()
	require.Equal(t, "nmi", l.price.Provider.PSP)
	require.NotEmpty(t, l.price.Provider.IDs["plan_id"])

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
	require.Equal(t, "merchant", w.nmi.LastSale().InitiatedBy)
	require.NotEmpty(t, w.nmi.LastSale().Initial, "manual_rebill uses the accepted recurring agreement")
	require.Empty(t, w.providerCatalogWrites())

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

	w.nmi.DeleteSchedule(railSub)
}

// A provider cadence is cached independently of access and a one-off override.
// A transient read failure keeps that cadence when the override is removed.
func TestProviderPriceCachedTermsKeepTheProviderCadence(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "provider-override", DisplayName: "Provider override"})
	require.NoError(t, err)
	plan := "override-" + uuid.NewString()[:8]
	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Override", Amount: "9.99", Days: 30})
	decl := catalog.ApplyProviderPrice{PSP: "nmi", PlanID: plan, OneOff: true, AccessDurationHours: catalog.Value(14 * 24)}
	apply := func() (*billing.CatalogApplicationReceipt, error) {
		return c.ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{product.Key: {ProviderPrices: map[string]catalog.ApplyProviderPrice{"legacy": decl}}}}, billing.ApplyCatalogParams{})
	}
	_, err = apply()
	require.NoError(t, err)
	first, err := priceByKey(t.Context(), c, product.Key, "legacy")
	require.NoError(t, err)
	require.Nil(t, first.BillingIntervalHours)
	verified, err := c.GetPrice(t.Context(), first.ID, billing.GetPriceParams{Verify: true})
	require.NoError(t, err)
	require.Equal(t, billing.SyncStatusInSync, verified.Provider.SyncStatus, "access override is not provider drift")
	decl.OneOff = false
	w.nmi.FailRequests(func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/plans/"+plan)
	}, http.StatusBadGateway, 30)
	receipt, err := apply()
	require.NoError(t, err)
	require.Len(t, receipt.Warnings, 1)
	require.False(t, receipt.Warnings[0].Refused, "same provider keeps cached terms")
	current, err := priceByKey(t.Context(), c, product.Key, "legacy")
	require.NoError(t, err)
	require.Equal(t, new(monthHours), current.BillingIntervalHours)
	require.Equal(t, new(14*24), current.AccessDurationHours)
	require.Equal(t, int64(9_990_000), current.UnitAmount)
	require.NotEqual(t, first.ID, current.ID, "changed billing is a new immutable version")
	earlier, err := c.GetPrice(t.Context(), first.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Equal(t, first.Provider.PSPID, earlier.Provider.PSPID)
	require.Equal(t, first.Provider.IDs, earlier.Provider.IDs, "older version retains its exact provider object")

}

// The upgrade's preserved provider identity is not the normal new-price hash.
// Reapplying equal terms must reuse it rather than collide on the financial tuple.
func TestProviderPriceAdoptionPreservesItsExistingIdentity(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "split-id", DisplayName: "Split identity"})
	require.NoError(t, err)
	plan := "adopt-" + uuid.NewString()[:8]
	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Adopt", Amount: "23.00", Days: 30})
	preserved := uuid.New()
	_, err = w.pool.Exec(t.Context(), w.sql(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived,access_duration_hours,billing_interval_hours,provider_owned,provider_psp_id,provider_snapshot) VALUES($1,$2,$3,'monthly-nmi',23000000,'USD',false,720,720,true,$4,$5::jsonb)`), c.MerchantID().UUID(), preserved, product.ID.UUID(), w.psp["nmi"].UUID(), fmt.Sprintf(`{"psp_key":"nmi","psp_id":"%s","rail":"nmi","plan_id":"%s","provider_billing_interval_hours":"720","provider_billing_interval_months":"none"}`, w.psp["nmi"].UUID(), plan))
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), w.sql(`INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,plan_id,configuration) VALUES($1,$2,$3,$4,'{"provider_billing_interval_hours":"720"}')`), c.MerchantID().UUID(), preserved, w.psp["nmi"].UUID(), plan)
	require.NoError(t, err)
	before, err := priceByKey(t.Context(), c, product.Key, "monthly-nmi")
	require.NoError(t, err)
	receipt, err := c.ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{product.Key: {ProviderPrices: map[string]catalog.ApplyProviderPrice{"monthly-nmi": {PSP: "nmi", PlanID: plan, AccessDurationHours: catalog.Value(monthHours)}}}}}, billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Zero(t, receipt.PricesChanged)
	after, err := priceByKey(t.Context(), c, product.Key, "monthly-nmi")
	require.NoError(t, err)
	require.Equal(t, before.ID, after.ID, "migrated identity survives adoption")
	require.Empty(t, w.providerCatalogWrites())
}

// Native months remain calendar months. A default accepts the verified paid
// period, while a later explicit override creates another immutable version.
func TestCalendarProviderPriceRetainsOldIdentityAndAccessChoice(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "calendar", DisplayName: "Calendar", Entitlements: []string{"calendar-access"}})
	require.NoError(t, err)
	plan := "calendar-" + uuid.NewString()[:8]
	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Calendar", Amount: "23.00", Months: 1})
	params := billing.CreateProviderPriceParams{ProductID: product.ID, Key: "monthly", PSP: "nmi", PlanID: plan}
	original, err := client.CreateProviderPrice(t.Context(), params)
	require.NoError(t, err)
	require.Nil(t, original.BillingIntervalHours)
	require.Equal(t, new(1), original.BillingIntervalMonths)
	require.True(t, original.Provider.AccessDurationDefault)
	now := w.clock.Now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	importAt := func(price *billing.Price) billing.SubscriptionID {
		t.Helper()
		c := w.newCustomer()
		vault := w.nmi.AddVault(visa)
		schedule := w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: plan, Amount: "23.00", Months: 1, NextBilling: end, Created: start})
		sale := w.nmi.AddSale(nmimock.Sale{OrderID: "calendar-" + schedule, Vault: vault, Amount: "23.00", At: start})
		book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}, Customers: []billing.DeclaredCustomer{{Customer: c.cid()}},
			PaymentMethods: []billing.DeclaredPaymentMethod{{Customer: c.cid(), Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.Vault(vault).BillingID, Card: declaredCard(visa)}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: schedule, Customer: c.cid(), Price: price.ID, Rail: "nmi", RailSubscriptionID: schedule, StartedAt: start, PaidThrough: &end,
				PaymentMethod:    &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.Vault(vault).BillingID},
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: "calendar:" + schedule, Amount: 23_000_000, Currency: "USD", IntervalMonths: 1, FirstDueAt: start}}},
			Transactions: []billing.DeclaredTransaction{{RailSubscriptionID: schedule, TransactionID: sale.TransactionID, Success: true, Amount: 23_000_000, Currency: "USD", OccurredAt: start, NextBillingAt: &end}}}
		result, err := client.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Len(t, result.Imported, 1, "%+v", result)
		subs, err := client.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: c.cid()})
		require.NoError(t, err)
		require.Len(t, subs.Items, 1)
		return subs.Items[0].ID
	}
	firstSub := importAt(original)
	params.AccessDurationHours = catalog.Value(14 * 24)
	overridden, err := client.CreateProviderPrice(t.Context(), params)
	require.NoError(t, err)
	require.NotEqual(t, original.ID, overridden.ID)
	require.False(t, overridden.Provider.AccessDurationDefault)
	secondSub := importAt(overridden)
	for _, expected := range []struct {
		id            billing.SubscriptionID
		accessEnd     time.Time
		defaultAccess bool
	}{{firstSub, end, true}, {secondSub, start.Add(14 * 24 * time.Hour), false}} {
		var gotEnd time.Time
		var choice bool
		require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT a.ends_at,s.provider_access_default_snapshot FROM billing.product_access a JOIN billing.subscriptions s ON s.merchant_id=a.merchant_id AND a.source_id=s.id::text WHERE s.id=$1 AND a.revoked_at IS NULL ORDER BY a.created_at DESC LIMIT 1`), expected.id.UUID()).Scan(&gotEnd, &choice))
		require.Equal(t, expected.accessEnd, gotEnd.UTC())
		require.Equal(t, expected.defaultAccess, choice)
	}
	earlier, err := client.GetPrice(t.Context(), original.ID, billing.GetPriceParams{Verify: true})
	require.NoError(t, err)
	require.Equal(t, original.Provider.PSPID, earlier.Provider.PSPID)
	require.Equal(t, original.Provider.IDs, earlier.Provider.IDs)
	require.True(t, earlier.Provider.AccessDurationDefault)
	require.Equal(t, billing.SyncStatusInSync, earlier.Provider.SyncStatus)
}
