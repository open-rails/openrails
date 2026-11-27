//go:build integration

package critical_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/authkit/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
)

// An import brings the merchant's history: a subscription that ended before
// the import lands canceled at the provider's own dates and for its reason,
// with the charges it paid, and nothing is canceled at the provider (the
// world's cleanup proves no write).
func TestPSPImportHistory(t *testing.T) {
	ended := func(t *testing.T, sub billing.Subscription, started, end, canceled time.Time, kind string) {
		t.Helper()
		require.Equal(t, billing.SubscriptionCanceled, sub.Status)
		require.True(t, started.Equal(sub.StartedAt), "started %s, want %s", sub.StartedAt, started)
		require.NotNil(t, sub.EndedAt)
		require.True(t, end.Equal(*sub.EndedAt), "ended %s, want %s", *sub.EndedAt, end)
		require.NotNil(t, sub.CanceledAt)
		require.True(t, canceled.Equal(*sub.CanceledAt), "canceled %s, want %s", *sub.CanceledAt, canceled)
		require.NotNil(t, sub.CancelType)
		require.Equal(t, kind, *sub.CancelType)
	}
	only := func(t *testing.T, iw *importWorld, who billing.CustomerID) billing.Subscription {
		t.Helper()
		subs := iw.subscriptions(t, who)
		require.Len(t, subs, 1)
		return subs[0]
	}
	paid := func(t *testing.T, iw *importWorld, who billing.CustomerID) []int64 {
		t.Helper()
		page, err := iw.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: who, PageRequest: billing.PageRequest{Limit: 100}})
		require.NoError(t, err)
		var out []int64
		for _, p := range page.Items {
			out = append(out, p.Amount)
		}
		slices.Sort(out)
		return out
	}

	t.Run("OR-J38-S14/stripe-ended-subscriptions-land-at-their-own-dates-for-their-reason", func(t *testing.T) {
		// The world starts in the past: a payment is never dated ahead of the
		// database's clock.
		iw := bootImportWorldDeclaring(t, true, func(w *world) { w.clock.Advance(-250 * 24 * time.Hour) }, "stripe")
		iw.api = iw.alpha
		iw.stripe.AddProduct("prod_basic", "Basic")
		iw.stripe.AddPrice(stripemock.Price{ID: "price_basic", Product: "prod_basic", Amount: 999, Interval: "month"})
		iw.stripe.AddPrice(stripemock.Price{ID: "price_plus", Product: "prod_basic", Amount: 1499, Interval: "month"})
		carol := authtest.NewUser(t, iw.auth)
		day := 24 * time.Hour
		t0 := iw.clock.Now().Truncate(time.Second)
		at := func(d time.Duration) { iw.clock.Advance(t0.Add(d).Sub(iw.clock.Now())) }
		// Alice pays two months, moves to Plus, pays it, then her renewal
		// fails and Stripe ends the subscription for it.
		alice := iw.stripe.AddSubscription(iw.stripe.AddCustomer(iw.alice.Email, "Alice", nil), "pm_alice", "price_basic", 999, t0, t0.Add(30*day))
		// Carol pays two months, then goes unpaid: Stripe keeps cutting draft
		// invoices and never states an end.
		carolSub := iw.stripe.AddSubscription(iw.stripe.AddCustomer(carol.Email, "Carol", nil), "pm_carol", "price_basic", 999, t0, t0.Add(30*day))
		// Bob subscribes, then cancels at once mid-period.
		at(5 * day)
		bob := iw.stripe.AddSubscription(iw.stripe.AddCustomer(iw.bob.Email, "Bob", nil), "pm_bob", "price_basic", 999, t0.Add(5*day), t0.Add(35*day))
		at(15 * day)
		iw.stripe.CancelSubscription(bob)
		at(30 * day)
		iw.stripe.RenewSubscription(alice, true)
		iw.stripe.RenewSubscription(carolSub, true)
		at(40 * day)
		iw.stripe.PortalPriceChangePaid(alice, "price_plus", 1499, 500)
		at(60 * day)
		iw.stripe.RenewSubscription(alice, true)
		iw.stripe.RenewSubscription(carolSub, false)
		at(65 * day)
		iw.stripe.SetSubscriptionStatus(carolSub, "unpaid", false)
		at(90 * day)
		iw.stripe.RenewSubscription(alice, false)
		iw.stripe.DraftRenewal(carolSub)
		at(95 * day)
		iw.stripe.EndSubscription(alice, "payment_failed")
		at(200 * day)
		iw.start(t)

		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}})
		_, err := iw.api.CreateProduct(t.Context(), billing.CreateProductParams{Key: "basic", DisplayName: "Basic", Entitlements: []string{"member:basic"}})
		require.NoError(t, err)
		prices := map[string]*billing.Price{}
		for _, stripePrice := range []string{"price_basic", "price_plus"} {
			prices[stripePrice], err = iw.api.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: "basic", Key: "stripe-" + stripePrice, PSP: "stripe", PriceID: stripePrice})
			require.NoError(t, err)
		}
		require.NoError(t, iw.link(t, imp.ID, billing.LinkPSPImportParams{}))
		recs := iw.apply(t, imp.ID)
		for ref, r := range recs {
			require.Equal(t, billing.PSPImportImported, r.State, "%s %s: %v", r.Kind, ref, deref(r.Reason))
		}
		require.Empty(t, iw.findingErrors(t, "pull.bill.unresolved", "pull.charge.after_cancellation"), "every charge lands, none after its cancel")

		a := only(t, iw, customerOf(iw.alice))
		ended(t, a, t0, t0.Add(95*day), t0.Add(95*day), "provider")
		require.Equal(t, prices["price_plus"].ID, a.PriceID, "it ended on the price it last billed")
		b := only(t, iw, customerOf(iw.bob))
		ended(t, b, t0.Add(5*day), t0.Add(15*day), t0.Add(15*day), "user")
		c := only(t, iw, customerOf(carol))
		ended(t, c, t0, t0.Add(60*day), t0.Add(60*day), "provider")

		require.Equal(t, []int64{5_000_000, 9_990_000, 9_990_000, 14_990_000}, paid(t, iw, customerOf(iw.alice)))
		// Alice's bills date her price change: two on Basic, then the
		// proration and a renewal on Plus.
		invoices, err := iw.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{SubscriptionID: a.ID, PageRequest: billing.PageRequest{Limit: 100}})
		require.NoError(t, err)
		billed := map[billing.PriceID][]int64{}
		for _, inv := range invoices.Items {
			require.Equal(t, billing.InvoiceComplete, inv.Status)
			for _, line := range inv.Lines {
				require.NotNil(t, line.PriceID)
				billed[*line.PriceID] = append(billed[*line.PriceID], line.Amount)
			}
		}
		for _, amounts := range billed {
			slices.Sort(amounts)
		}
		require.Equal(t, map[billing.PriceID][]int64{prices["price_basic"].ID: {9_990_000, 9_990_000}, prices["price_plus"].ID: {5_000_000, 14_990_000}}, billed)
		require.Equal(t, []int64{9_990_000}, paid(t, iw, customerOf(iw.bob)))
		require.Equal(t, []int64{9_990_000, 9_990_000}, paid(t, iw, customerOf(carol)))
		for _, who := range []billing.CustomerID{customerOf(iw.alice), customerOf(iw.bob), customerOf(carol)} {
			require.False(t, iw.entitled(t, who, "member:basic"), "an ended subscription grants nothing now")
		}
	})

	t.Run("OR-J38-S15/ccbill-members-who-left-land-from-signup-to-expiry-with-their-rebills", func(t *testing.T) {
		iw := bootImportWorld(t, "ccbill")
		iw.api = iw.alpha
		day := 24 * time.Hour
		now := iw.clock.Now()
		t0 := now.Add(-120 * day).Truncate(day).Add(10 * time.Hour)
		dayEnd := func(d time.Duration) time.Time { return t0.Add(d).Truncate(day).Add(day - time.Second) }
		dayOf := func(d time.Duration) time.Time { return t0.Add(d).Truncate(day) }
		dana, erin, frank, gina := authtest.NewUser(t, iw.auth), authtest.NewUser(t, iw.auth), authtest.NewUser(t, iw.auth), authtest.NewUser(t, iw.auth)
		signup := func(u authtest.User, d time.Duration) string {
			return iw.ccbill.AddSignup(ccbillmock.Signup{Username: u.Username, Email: u.Email, At: t0.Add(d), Initial: "2.95", InitialDays: 2, Recurring: "23.00", RecurringDays: 30})
		}
		// Dana rebills twice, cancels, and her paid time runs out.
		danaSub := signup(dana, 0)
		for _, d := range []time.Duration{30 * day, 60 * day} {
			iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: danaSub, At: t0.Add(d), Amount: "23.00"})
		}
		iw.ccbill.AddCancellation(ccbillmock.Cancellation{SubscriptionID: danaSub, At: t0.Add(75 * day), Expires: t0.Add(90 * day)})
		iw.ccbill.AddExpiry(ccbillmock.Expiry{SubscriptionID: danaSub, Expires: t0.Add(90 * day), Canceled: ptr(t0.Add(75 * day))})
		// Erin's second rebill fails: she expires without canceling.
		erinSub := signup(erin, 5*day)
		iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: erinSub, At: t0.Add(35 * day), Amount: "23.00"})
		iw.ccbill.AddExpiry(ccbillmock.Expiry{SubscriptionID: erinSub, Expires: t0.Add(65 * day)})
		// Frank cancels in his first days, never rebilling: his signup's
		// terms price him.
		frankSub := signup(frank, 10*day)
		iw.ccbill.AddCancellation(ccbillmock.Cancellation{SubscriptionID: frankSub, At: t0.Add(11 * day), Expires: t0.Add(12 * day)})
		iw.ccbill.AddExpiry(ccbillmock.Expiry{SubscriptionID: frankSub, Expires: t0.Add(12 * day), Canceled: ptr(t0.Add(11 * day))})
		// Gina is still a member.
		next := now.Add(10 * day)
		ginaSub := iw.ccbill.AddMember(ccbillmock.Member{Username: gina.Username, Email: gina.Email, Signup: t0.Add(20 * day), Rebill: &next})
		iw.ccbill.AddSignup(ccbillmock.Signup{SubscriptionID: ginaSub, Username: gina.Username, Email: gina.Email, At: t0.Add(20 * day), Recurring: "23.00", RecurringDays: 30})
		iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: ginaSub, At: now.Add(-20 * day), Amount: "23.00"})
		iw.start(t)

		// Members who left are read only as far back as the history.
		since := t0.Add(-time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "ccbill"}, HistorySince: &since})
		_, err := iw.api.CreateProduct(t.Context(), billing.CreateProductParams{Key: "premium", DisplayName: "Premium", Entitlements: []string{"member:premium"}})
		require.NoError(t, err)
		premium, err := iw.api.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: "premium", Key: "ccbill-23", PSP: "ccbill",
			FlexID: "681cb38f-afb9-4665-931f-2b896072178a", RecurringBillingOptionID: "0000000023", UnitAmount: ptr(int64(23_000_000)), Currency: "USD", BillingIntervalHours: ptr(720)})
		require.NoError(t, err)
		require.NoError(t, iw.link(t, imp.ID, billing.LinkPSPImportParams{}))
		recs := iw.apply(t, imp.ID)
		for _, ref := range []string{danaSub, erinSub, frankSub, ginaSub} {
			require.Equal(t, billing.PSPImportImported, recs[ref].State, "%s: %v", ref, deref(recs[ref].Reason))
		}

		d := only(t, iw, customerOf(dana))
		ended(t, d, t0, dayEnd(90*day), dayOf(75*day), "user")
		require.Equal(t, premium.ID, d.PriceID)
		require.Equal(t, []int64{23_000_000, 23_000_000}, paid(t, iw, customerOf(dana)), "her rebills are hers")
		e := only(t, iw, customerOf(erin))
		ended(t, e, t0.Add(5*day), dayEnd(65*day), dayEnd(65*day), "provider")
		require.Equal(t, []int64{23_000_000}, paid(t, iw, customerOf(erin)))
		f := only(t, iw, customerOf(frank))
		ended(t, f, t0.Add(10*day), dayEnd(12*day), dayOf(11*day), "user")
		require.Equal(t, premium.ID, f.PriceID, "priced by the terms he signed up for")
		require.Empty(t, paid(t, iw, customerOf(frank)))
		for _, who := range []billing.CustomerID{customerOf(dana), customerOf(erin), customerOf(frank)} {
			require.False(t, iw.entitled(t, who, "member:premium"))
		}
		g := only(t, iw, customerOf(gina))
		require.Equal(t, billing.SubscriptionActive, g.Status)
		require.True(t, iw.entitled(t, customerOf(gina), "member:premium"))

		// One request a day reads every kind of history.
		exports := 0
		for _, r := range iw.ccbill.Requests() {
			if r.Start.IsZero() {
				continue
			}
			exports++
			require.Equal(t, "NEW,REBILL,CANCELLATION,EXPIRE", r.Types)
			require.LessOrEqual(t, r.End.Sub(r.Start), day-time.Second)
		}
		require.Positive(t, exports)
	})

	t.Run("OR-J38-S16/an-nmi-schedule-paused-long-ago-ends-where-its-paid-time-ended", func(t *testing.T) {
		iw := bootImportWorld(t, "nmi")
		iw.api = iw.alpha
		day := 24 * time.Hour
		start := iw.clock.Now().Add(-150 * day).Truncate(day)
		through := start.Add(60 * day)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		vault := iw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		iw.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Email = iw.alice.Email })
		schedule := iw.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: "gold", Amount: "20.00", Days: 30, Created: start, NextBilling: through,
			Order: "invoice-gold-" + vault, Paused: true})
		iw.nmi.AddScheduleSale(schedule, start)
		iw.nmi.AddScheduleSale(schedule, start.Add(30*day))
		iw.start(t)

		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}})
		_, err := iw.api.CreateProduct(t.Context(), billing.CreateProductParams{Key: "gold", DisplayName: "Gold", Entitlements: []string{"member:gold"}})
		require.NoError(t, err)
		_, err = iw.api.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: "gold", Key: "nmi-gold", PSP: "nmi", PlanID: "gold"})
		require.NoError(t, err)
		require.NoError(t, iw.link(t, imp.ID, billing.LinkPSPImportParams{}))
		recs := iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[schedule].State, "%v", deref(recs[schedule].Reason))

		sub := only(t, iw, customerOf(iw.alice))
		ended(t, sub, start, through, through, "user")
		require.Equal(t, []int64{20_000_000, 20_000_000}, paid(t, iw, customerOf(iw.alice)))
		require.False(t, iw.entitled(t, customerOf(iw.alice), "member:gold"))
	})
}

// findingErrors lists the merchant's findings of types, each with the error
// its evidence names.
func (iw *importWorld) findingErrors(t *testing.T, types ...string) string {
	t.Helper()
	var out string
	require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT COALESCE(string_agg(finding_type || ' ' || subject_key || ' ' || COALESCE(evidence->>'error', ''), E'\n' ORDER BY finding_type, subject_key), '')
		FROM %s WHERE merchant_id = $1 AND finding_type = ANY($2)`, iw.table("reconciliation_findings")), iw.alpha.MerchantID().UUID(), types).Scan(&out))
	return out
}
