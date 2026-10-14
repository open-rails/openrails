//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest"
)

// A merchant's Stripe customers import straight from the account: emails
// link through the directory, a metadata user id names the customer
// itself, a canceled subscription lands canceled, and the invoices Stripe
// charged land as payments. The pull writes nothing to Stripe.
func TestPSPImportFromStripe(t *testing.T) {
	t.Parallel()
	directory := &openrailstest.UserInfo{}
	w := prepareWorld(t, 12)
	w.deps = func(d *openrails.Deps) { d.UserInfo = directory }
	w.start()
	w.waive("recorded", "the unmatched customer's charge stays staged, never landed")
	ctx := t.Context()
	client := w.client[remote]
	writes := len(w.stripe.Mutations(""))

	entitlement := "content:stripe-" + uuid.NewString()[:6]
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "premium-" + uuid.NewString()[:8], DisplayName: "Premium", Entitlements: []string{entitlement}})
	require.NoError(t, err)
	price := "price_legacy_" + uuid.NewString()[:8]

	start := w.clock.Now().Add(-10 * 24 * time.Hour)
	end := start.AddDate(0, 1, 0)
	a, b, c := w.newCustomer(), w.newCustomer(), w.newCustomer()
	directory.Put(userinfo.User{ID: a.id, Email: "a@host.test"})
	directory.Put(userinfo.User{ID: c.id, Email: "c@host.test"})
	subscribe := func(customer string) (string, string) {
		method := "pm_" + uuid.NewString()[:8]
		sub := w.stripe.AddSubscription(customer, method, price, 999, start, end)
		return sub, w.stripe.LatestCharge(sub)
	}
	subA, chargeA := subscribe(w.stripe.AddCustomer("A@host.test", "A", nil))
	subB, _ := subscribe(w.stripe.AddCustomer("", "B", map[string]string{"user_id": b.id}))
	subC, _ := subscribe(w.stripe.AddCustomer("c@host.test", "C", nil))
	w.stripe.CancelSubscription(subC)
	subNobody, _ := subscribe(w.stripe.AddCustomer("nobody@elsewhere.test", "", nil))
	w.advance(time.Minute)

	since := start.Add(-24 * time.Hour)
	imp, err := w.client[embedded].CreatePSPImport(ctx, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}, HistorySince: &since,
		Rules: []billing.PSPImportRule{
			{Fact: "email", Via: billing.PSPImportViaDirectory},
			{Fact: "metadata.user_id", Via: billing.PSPImportViaCustomerID},
		},
		Products: []billing.PSPImportProduct{{Plan: price, Product: product.Key}}})
	require.NoError(t, err)
	imp = w.pspImported(imp.ID, billing.PSPImportPulled)
	recs := w.pspRecords(imp.ID)
	require.Equal(t, a.cid(), *recs[subA].Customer)
	require.Equal(t, b.cid(), *recs[subB].Customer)
	require.Equal(t, "subscription:"+subA, *recs[chargeA].LinkedVia, "a charge belongs to the subscription its invoice bills")
	require.Equal(t, billing.PSPImportUnmatched, recs[subNobody].State)

	_, err = client.ApplyPSPImport(ctx, imp.ID)
	require.NoError(t, err)
	imp = w.pspImported(imp.ID, billing.PSPImportApplied)

	prices, err := client.ListPrices(ctx, billing.PriceListParams{ProductID: product.ID})
	require.NoError(t, err)
	require.Len(t, prices.Items, 1)
	require.True(t, prices.Items[0].Archived)
	require.Equal(t, int64(9_990_000), prices.Items[0].UnitAmount, "terms read from the Stripe price")

	for _, m := range []struct {
		who    *customer
		sub    string
		status string
	}{{a, subA, "active"}, {b, subB, "active"}, {c, subC, "canceled"}} {
		subs, err := client.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: m.who.cid()})
		require.NoError(t, err)
		require.Len(t, subs.Items, 1, "%s: %+v", m.sub, w.pspRecords(imp.ID)[m.sub])
		require.Equal(t, m.sub, str(subs.Items[0].RailSubscriptionID))
		require.Equal(t, m.status, string(subs.Items[0].Status))
		methods, err := client.ListPaymentMethods(ctx, m.who.cid(), billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Len(t, methods.Items, 1, "%+v", w.pspRecords(imp.ID))
		require.Equal(t, "4242", *methods.Items[0].Card.Last4)
	}
	require.True(t, a.entitled(entitlement))
	payments, err := client.ListPayments(ctx, billing.PaymentListParams{CustomerID: a.cid(), TransactionID: chargeA})
	require.NoError(t, err)
	require.Len(t, payments.Items, 1, "the invoice's charge is a payment")
	require.Equal(t, int64(9_990_000), payments.Items[0].Amount)
	recs = w.pspRecords(imp.ID)
	require.Equal(t, billing.PSPImportImported, recs[chargeA].State)
	require.Equal(t, billing.PSPImportUnmatched, recs[subNobody].State)

	require.Len(t, w.stripe.Mutations(""), writes, "the import only read Stripe")
}
