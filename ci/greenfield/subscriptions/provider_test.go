//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed/operator"
)

// legacy is one imported provider-owned membership: the provider owns the
// schedule and OpenRails mirrors it.
type legacy struct {
	w        *world
	rail     string
	tp       topology
	c        *customer
	price    *openrails.Price
	railSub  string
	sub      openrails.SubscriptionID
	ent      string
	railCust string
}

// importLegacy creates a provider subscription at the fake provider and
// lands it through ImportBilling, the documented legacy-book entry point.
func importLegacy(t *testing.T, w *world, rail string, tp topology, configure ...func(*openrails.DeclaredBilling)) *legacy {
	t.Helper()
	l := &legacy{w: w, rail: rail, tp: tp, ent: "content:legacy", c: w.newCustomer()}
	client := w.client[tp]
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "legacy-" + uuid.NewString()[:8], DisplayName: "Legacy membership", EntitlementsSpec: map[string]*int{l.ent: nil}})
	require.NoError(t, err)
	hours := monthHours
	links := map[string]map[string]string{"stripe": {"price_id": "price_legacy_" + uuid.NewString()[:8]}}
	if rail == "nmi" {
		links = map[string]map[string]string{"nmi": {"plan_id": "legacy_plan_" + uuid.NewString()[:8]}}
	}
	l.price, err = client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours, PSPLinks: links})
	require.NoError(t, err)

	start := w.clock.Now().Add(-10 * day)
	end := start.Add(monthHours * time.Hour)
	customerID, err := openrails.ParseCustomerID(l.c.id)
	require.NoError(t, err)
	priceID, err := openrails.ParsePriceID(l.price.ID)
	require.NoError(t, err)
	book := openrails.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: openrails.PSPRef{Key: rail}, Customers: []openrails.DeclaredCustomer{{Customer: customerID}}}
	switch rail {
	case "stripe":
		l.railCust = "cus_legacy" + uuid.NewString()[:8]
		method := "pm_legacy" + uuid.NewString()[:8]
		l.railSub = w.stripe.legacySubscription(l.railCust, method, links["stripe"]["price_id"], 999, start, end)
		book.PaymentMethods = []openrails.DeclaredPaymentMethod{{Customer: customerID, Rail: "stripe", RailCustomerRef: l.railCust, RailMethodRef: method, LastFour: "4242", CardType: "visa", ExpiryDate: "12/35"}}
		book.Subscriptions = []openrails.DeclaredSubscription{{SourceID: "legacy-" + l.railSub, Customer: customerID, Price: priceID, Rail: "stripe", RailSubscriptionID: l.railSub, StartedAt: start, PaidThrough: &end,
			PaymentMethod: &openrails.PaymentMethodRef{Rail: "stripe", RailCustomerRef: l.railCust, RailMethodRef: method}}}
		book.Transactions = []openrails.DeclaredTransaction{{RailSubscriptionID: l.railSub, TransactionID: w.stripe.latestCharge(l.railSub), Success: true, AmountCents: 999, Currency: "USD", OccurredAt: start}}
	case "nmi":
		vault := w.nmi.legacyVault(visa)
		l.railCust = vault
		l.railSub = w.nmi.legacySchedule(vault, links["nmi"]["plan_id"], "9.99", end)
		book.PaymentMethods = []openrails.DeclaredPaymentMethod{{Customer: customerID, Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.billingOf(vault), LastFour: "4242", CardType: "visa", ExpiryDate: "12/35"}}
		book.Subscriptions = []openrails.DeclaredSubscription{{SourceID: "legacy-" + l.railSub, Customer: customerID, Price: priceID, Rail: "nmi", RailSubscriptionID: l.railSub, StartedAt: start, PaidThrough: &end,
			PaymentMethod: &openrails.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.billingOf(vault)}}}
		book.Transactions = []openrails.DeclaredTransaction{{RailSubscriptionID: l.railSub, TransactionID: w.nmi.legacySale(vault, "9.99", start), Success: true, AmountCents: 999, Currency: "USD", OccurredAt: start}}
	}
	book.Subscriptions[0].CollectionPolicy = "provider"
	for _, apply := range configure {
		apply(&book)
	}
	policy := book.Subscriptions[0].CollectionPolicy
	if rail == "nmi" {
		book.PaymentMethods[0].InitialTransactionID = book.Transactions[0].TransactionID
		if policy == "provider_dunning" {
			book.PaymentMethods[0].RecurringTransactionID = book.Transactions[0].TransactionID
		}
		if book.Subscriptions[0].Dunning != nil {
			w.nmi.mu.Lock()
			w.nmi.schedules[l.railSub].NextBilling = book.Subscriptions[0].PaidThrough.Add(monthHours * time.Hour)
			w.nmi.mu.Unlock()
		}
	}
	result, err := client.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Len(t, result.Imported, 1, "%+v", result)
	w.settle()
	subs, err := client.ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: l.c.id})
	require.NoError(t, err)
	require.Len(t, subs.Data, 1)
	l.sub = subs.Data[0].ID
	sub := subs.Data[0]
	status := "active"
	if d := book.Subscriptions[0].Dunning; d != nil {
		status = "past_due"
		require.NotNil(t, sub.RetryAttempts)
		require.Equal(t, d.Retries, *sub.RetryAttempts)
		require.NotNil(t, sub.GraceEndsAt)
	}
	require.Equal(t, status, sub.Status)
	require.Equal(t, l.railSub, sub.RailSubscriptionID)
	wantPolicy := policy
	if rail == "nmi" && (book.PaymentMethods[0].RecurringTransactionID != "" || scheduleSale(book, l.railSub)) {
		wantPolicy = "provider_dunning" // every NMI schedule with a recurring anchor is dunned by OpenRails (Paul, 2026-09-25)
	}
	require.Equal(t, wantPolicy, sub.CollectionPolicy)
	require.NotNil(t, sub.PaymentMethodID)
	charges := l.engineCharges()
	replay, err := client.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Equal(t, []string{book.Subscriptions[0].SourceID}, replay.Skipped)
	require.Equal(t, charges, l.engineCharges(), "import replay never charges")
	if policy == "provider_dunning" {
		conflict := book
		conflict.PaymentMethods = append([]openrails.DeclaredPaymentMethod(nil), book.PaymentMethods...)
		conflict.PaymentMethods[0].RecurringTransactionID = "another-recurring-agreement"
		_, err := client.ImportBilling(t.Context(), conflict)
		require.Error(t, err, "reimport cannot replace an accepted recurring agreement")
		require.Equal(t, charges, l.engineCharges())
	}
	return l
}

func (l *legacy) periodEnd() time.Time {
	return *l.w.subscription(l.tp, l.sub).CurrentPeriodEndsAt
}

// engineWrites counts provider mutations OpenRails could use to charge.
func (l *legacy) engineCharges() int {
	if l.rail == "stripe" {
		return len(l.w.stripe.mutations("/v1/payment_intents"))
	}
	return l.w.nmi.saleAttempts()
}

// Provider-owned mode: import, then provider renewals delivered duplicated,
// out of order and late move the local period and payments exactly once;
// OpenRails never charges on its own; entitlement stays continuous.
func TestProviderOwnedRenewals(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		l := importLegacy(t, w, rail, tp)
		w.converge()
		require.True(t, l.c.entitled(l.ent), "an imported paid membership grants access")
		charges := l.engineCharges()

		// OpenRails never rebills a provider-owned schedule, even past its
		// period end with no provider news.
		end := l.periodEnd()
		w.advance(end.Sub(w.clock.Now()) + time.Hour)
		w.runRenewals()
		require.Equal(t, charges, l.engineCharges(), "no engine charge for a provider-owned subscription")
		require.True(t, l.c.entitled(l.ent), "standing access while the provider bills")

		var first obj
		if rail == "stripe" {
			// Stripe first rolls the period with a draft invoice; that is a
			// renewal in progress, never a decline.
			require.Equal(t, http.StatusOK, w.deliver(rail, stripeEvent("customer.subscription.updated", w.stripe.providerDraft(l.railSub))))
			sub := w.subscription(tp, l.sub)
			require.Equal(t, "active", sub.Status, "a draft invoice is not a failed renewal")
			for _, p := range w.payments(tp, l.c.id) {
				require.NotEqual(t, "failed", p.Status, "no failed payment for a draft invoice")
			}
			first = stripeEvent("invoice.paid", w.stripe.providerCollectDraft(l.railSub))
		} else {
			first = l.providerRenewal(true)
		}
		require.Equal(t, http.StatusOK, w.deliver(rail, first))
		require.Equal(t, http.StatusOK, w.deliver(rail, first), "a duplicate delivery")
		renewed := l.periodEnd()
		require.True(t, renewed.After(end), "the provider renewal moves the local period")
		paid := len(completed(w.payments(tp, l.c.id)))

		// A late, stale notice for the old period changes nothing.
		require.Equal(t, http.StatusOK, w.deliver(rail, l.staleNotice()))
		require.True(t, l.periodEnd().Equal(renewed))
		require.Len(t, completed(w.payments(tp, l.c.id)), paid, "each provider charge is recorded once")

		// A second renewal delivered only after a delay still lands once.
		w.advance(renewed.Sub(w.clock.Now()) + 2*day)
		second := l.providerRenewal(true)
		require.Equal(t, http.StatusOK, w.deliver(rail, second))
		require.True(t, l.periodEnd().After(renewed))
		require.Len(t, completed(w.payments(tp, l.c.id)), paid+1)
		require.Equal(t, charges, l.engineCharges())
		require.True(t, l.c.entitled(l.ent))
	})
}

// providerRenewal is the provider charging its schedule and the notice it
// sends about it.
func (l *legacy) providerRenewal(paid bool) obj {
	if l.rail == "stripe" {
		inv := l.w.stripe.providerRenew(l.railSub, paid)
		kind := "invoice.paid"
		if !paid {
			kind = "invoice.payment_failed"
		}
		return stripeEvent(kind, inv)
	}
	sale, _ := l.w.nmi.providerRenew(l.railSub, paid)
	if !paid {
		return nmiEvent("transaction.sale.failure", obj{"transaction_id": sale.TransactionID, "transaction_type": "cc", "condition": "failed", "amount": "9.99", "currency": "USD", "customer_vault_id": l.railCust,
			"subscription": obj{"subscription_id": l.railSub}, "action": obj{"action_type": "sale", "amount": "9.99", "success": "0", "response_code": "202"}})
	}
	return nmiEvent("transaction.sale.success", obj{"transaction_id": sale.TransactionID, "transaction_type": "cc", "condition": "pendingsettlement", "amount": sale.Amount, "currency": "USD", "order_id": sale.OrderID, "customer_vault_id": sale.Vault,
		"subscription": obj{"subscription_id": l.railSub}, "action": obj{"action_type": "sale", "amount": sale.Amount, "success": "1", "response_code": "100"}})
}

// staleNotice is an old lifecycle notice arriving after newer ones.
func (l *legacy) staleNotice() obj {
	if l.rail == "stripe" {
		stale := l.w.stripe.subscriptionObject(l.railSub)
		event := stripeEvent("customer.subscription.updated", stale)
		event["created"] = time.Now().Add(-40 * day).Unix()
		return event
	}
	return nmiEvent("recurring.subscription.update", obj{"subscription_id": l.railSub})
}

// converge is the documented post-import step (docs/batch-import.md): the
// operator's on-demand merchant convergence derives imported memberships'
// grants and entitlement windows.
func (w *world) converge() {
	w.t.Helper()
	res, err := operator.New(w.rt).Converge(w.t.Context(), w.client[embedded].MerchantID())
	require.NoError(w.t, err)
	w.t.Logf("converge: %+v", res)
	w.settle()
}

// Provider-owned lifecycle: cancelling through OpenRails cancels at the
// provider; a provider-side cancel or failed payment is mirrored locally;
// the host's account-deletion cancel works whatever state the mirror is in.
func TestProviderOwnedLifecycle(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, rail string, tp topology) {
		t.Run("cancel_via_openrails", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importLegacy(t, w, rail, tp)
			w.converge()
			require.NoError(t, w.client[tp].CancelSubscription(t.Context(), l.sub, openrails.CancelSubscriptionRequest{Reason: "member asked"}))
			w.settle()
			w.advance(time.Hour)
			w.wake()
			require.NotNil(t, w.subscription(tp, l.sub).CancelledAt)
			if rail == "stripe" {
				require.Equal(t, true, w.stripe.subscriptionObject(l.railSub)["cancel_at_period_end"], "Stripe stops renewing")
			} else {
				require.False(t, w.nmi.scheduleLive(l.railSub), "the NMI schedule is deleted")
			}
			require.True(t, l.c.entitled(l.ent), "the paid period is kept")
		})
		t.Run("provider_cancel", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importLegacy(t, w, rail, tp)
			w.converge()
			require.Equal(t, http.StatusOK, w.deliver(rail, l.providerCancelNotice()))
			sub := w.subscription(tp, l.sub)
			require.Equal(t, "cancelled", sub.Status, "the provider's own cancellation is mirrored")
		})
		t.Run("provider_payment_failed", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			l := importLegacy(t, w, rail, tp)
			w.converge()
			w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
			require.Equal(t, http.StatusOK, w.deliver(rail, l.providerRenewal(false)))
			sub := w.subscription(tp, l.sub)
			require.Equal(t, "past_due", sub.Status, "the provider's failed renewal is mirrored")
			require.True(t, l.c.entitled(l.ent), "provider-owned dunning keeps standing access")
			charges := l.engineCharges()
			w.runRenewals()
			require.Equal(t, charges, l.engineCharges(), "OpenRails leaves the provider's dunning alone")
			// The host's account-deletion callback cancels what it finds.
			require.NoError(t, w.client[tp].CancelSubscription(t.Context(), l.sub, openrails.CancelSubscriptionRequest{Reason: "Account deletion evt_2", AccountDeletion: true}))
			require.NotNil(t, w.subscription(tp, l.sub).CancelledAt)
			w.advance(time.Hour)
			w.wake()
			if rail == "stripe" {
				require.Equal(t, "canceled", w.stripe.subscriptionObject(l.railSub)["status"], "a delinquent Stripe schedule ends now, so its open invoice stops retrying")
			} else {
				// Documented: the destructive-action switch ships off and holds
				// every NMI schedule delete until an operator arms it.
				require.True(t, w.nmi.scheduleLive(l.railSub), "the delete waits for the operator's switch")
				w.armDestructive()
				w.advance(time.Hour)
				w.wake()
				require.False(t, w.nmi.scheduleLive(l.railSub), "the held delete runs once armed")
			}
		})
	})
}

// providerCancelNotice ends the schedule at the provider and returns its notice.
func (l *legacy) providerCancelNotice() obj {
	if l.rail == "stripe" {
		return stripeEvent("customer.subscription.deleted", l.w.stripe.providerCancel(l.railSub))
	}
	l.w.nmi.providerCancel(l.railSub)
	return nmiEvent("recurring.subscription.delete", obj{"subscription_id": l.railSub})
}

// NMI owns the schedule, while OpenRails owns recovery of a failed scheduled
// charge. NMI's next billing date advances even though the failed period is
// unpaid; the signed webhook causes OpenRails to fetch that provider truth.
func TestNMIProviderScheduleOpenRailsDunning(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			l := importLegacy(t, w, "nmi", tp, func(book *openrails.DeclaredBilling) {
				book.Subscriptions[0].CollectionPolicy = "provider_dunning"
			})
			w.converge()
			// Hold provider writes while observing the failed period. The due
			// pass runs autonomously and may otherwise recover it before readback.
			w.cfg = func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeReadOnly }
			w.restart()
			end := l.periodEnd()
			w.advance(end.Sub(w.clock.Now()) + time.Hour)
			require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))
			sub := w.subscription(tp, l.sub)
			require.Equal(t, "past_due", sub.Status)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(end), "a future schedule date cannot grant an unpaid period")
			require.NotNil(t, sub.NextRetryAt, "OpenRails schedules recovery after the provider decline")
			require.WithinDuration(t, w.clock.Now().Add(48*time.Hour), *sub.NextRetryAt, time.Second, "NMI never retries; OpenRails' first retry is the schedule's +2d, not now")
			require.Zero(t, w.nmi.saleAttempts(), "read-only posture holds automatic recovery")
			w.cfg = nil
			w.restart()
			w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
			w.runRenewals()
			sub = w.subscription(tp, l.sub)
			require.Equal(t, "active", sub.Status)
			require.Equal(t, "provider_dunning", sub.CollectionPolicy)
			require.Equal(t, l.railSub, sub.RailSubscriptionID)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(end.Add(monthHours*time.Hour)))
			require.Nil(t, sub.NextRetryAt)
			require.Equal(t, 1, w.nmi.saleAttempts(), "one automatic recovery charge")
			require.Len(t, w.nmi.ledger(""), 2, "initial and recovered periods each charged once")
			paid := completed(w.payments(tp, l.c.id))
			require.Len(t, paid, 2)
			recovery := w.nmi.lastSale()
			require.Equal(t, "9.99", recovery.Amount)
			require.Equal(t, "merchant", recovery.InitiatedBy)
			require.Equal(t, "used", recovery.Indicator)
			require.NotEmpty(t, recovery.Initial, "recovery reuses the imported recurring agreement")
			require.NotEqual(t, "another-recurring-agreement", recovery.Initial, "refused reimport did not replace the mandate")
			require.Equal(t, w.nmi.ledger("")[0].ID, recovery.Initial)
			matched := false
			for _, payment := range paid {
				if payment.TransactionID == recovery.TransactionID {
					require.EqualValues(t, 9_990_000, payment.Amount)
					require.Equal(t, "USD", payment.Currency)
					matched = true
				}
			}
			require.True(t, matched, "the recovery provider charge is recorded locally")
			require.True(t, w.nmi.scheduleLive(l.railSub), "NMI still owns the next scheduled charge")
			require.True(t, l.c.entitled(l.ent))
			w.runRenewals()
			require.Equal(t, 1, w.nmi.saleAttempts(), "a repeated due pass cannot recharge the recovered period")
			require.Empty(t, w.nmi.unexpected())
		})
	}
}

// Import preserves the source recovery history as well as ownership. It
// must not submit a fresh charge when restoring or replaying this book.
func TestNMIProviderDunningImportRetainsRetryHistory(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	importLegacy(t, w, "nmi", remote, func(book *openrails.DeclaredBilling) {
		last := w.clock.Now().Add(-2 * time.Hour)
		book.Subscriptions[0].CollectionPolicy = "provider_dunning"
		book.Subscriptions[0].Dunning = &openrails.DunningEvidence{Retries: 2, LastRetryAt: &last, ScheduleLive: true}
	})
	require.Zero(t, w.nmi.saleAttempts())
}

// A stalled OpenRails-dunned schedule (past_due, no retry scheduled) resumes
// at the dunning schedule's next step after its last attempt, never at once.
// Past grace it is left to grace_exhausted, and provider-owned Stripe and NMI
// rows are never given a retry.
func TestDunningStallResumesOnSchedule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	dunning := func(book *openrails.DeclaredBilling) { book.Subscriptions[0].CollectionPolicy = "provider_dunning" }
	stalled := importLegacy(t, w, "nmi", embedded, dunning)
	lapsed := importLegacy(t, w, "nmi", embedded, dunning)
	stripeOwned := importLegacy(t, w, "stripe", embedded)
	nmiOwned := importLegacy(t, w, "nmi", embedded)
	w.converge()
	charges := stripeOwned.engineCharges()
	w.advance(stalled.periodEnd().Sub(w.clock.Now()) + day)
	now := w.clock.Now()
	last := now.Add(-time.Hour)
	stall := func(l *legacy, grace time.Time) {
		_, err := w.pool.Exec(t.Context(), strings.ReplaceAll(`UPDATE openrails.subscriptions
			SET lifecycle_rev = lifecycle_rev + 1, status = 'past_due', next_retry_at = NULL, retry_attempts = 2, last_retry_at = $2, grace_ends_at = $3
			WHERE id = $1`, "openrails.", pgx.Identifier{w.schema}.Sanitize()+"."), l.sub.UUID(), last, grace)
		require.NoError(t, err)
	}
	stall(stalled, now.Add(10*day))
	stall(lapsed, now.Add(-time.Minute))
	stall(stripeOwned, now.Add(10*day))
	stall(nmiOwned, now.Add(10*day))

	w.converge()
	sub := w.subscription(embedded, stalled.sub)
	require.Equal(t, "past_due", sub.Status)
	require.NotNil(t, sub.NextRetryAt, "the stalled schedule resumes")
	require.WithinDuration(t, last.Add(3*day), *sub.NextRetryAt, time.Second, "third attempt: +5d after the first failure, 3d after the second")
	sub = w.subscription(embedded, lapsed.sub)
	require.Nil(t, sub.NextRetryAt, "past grace, grace_exhausted owns it")
	require.Equal(t, "unverified", sub.Status)
	sub = w.subscription(embedded, stripeOwned.sub)
	require.Nil(t, sub.NextRetryAt, "stripe: the provider owns its retries")
	require.Equal(t, "past_due", sub.Status)
	sub = w.subscription(embedded, nmiOwned.sub)
	require.NotNil(t, sub.NextRetryAt, "nmi: NMI never retries, so OpenRails dunning resumes")
	require.Equal(t, "past_due", sub.Status)
	w.runRenewals()
	require.Zero(t, w.nmi.saleAttempts())
	require.Equal(t, charges, stripeOwned.engineCharges())
}

// A lapsed provider_dunning schedule with no decline seen is never charged by
// OpenRails: NMI may have billed it unobserved. When NMI's renewal arrives
// late, the period is paid exactly once.
func TestNMIProviderDunningLapseWithoutDeclineNeverCharged(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded, func(book *openrails.DeclaredBilling) {
		book.Subscriptions[0].CollectionPolicy = "provider_dunning"
	})
	w.converge()
	end := l.periodEnd()
	for _, step := range []time.Duration{end.Sub(w.clock.Now()) + time.Hour, 49 * time.Hour} {
		w.advance(step)
		w.converge()
		w.runRenewals()
		sub := w.subscription(embedded, l.sub)
		require.NotEqual(t, "past_due", sub.Status, "a lapse is not a decline")
		require.Nil(t, sub.NextRetryAt)
		require.Zero(t, w.nmi.saleAttempts(), "OpenRails never charges without a seen decline")
	}

	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true)))
	w.runRenewals()
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, "active", sub.Status)
	require.True(t, sub.CurrentPeriodEndsAt.After(end))
	require.Zero(t, w.nmi.saleAttempts())
	require.Len(t, w.nmi.ledger(""), 2, "the initial and NMI's renewal, nothing more")
	require.Len(t, completed(w.payments(embedded, l.c.id)), 2)
}

// scheduleSale reports whether the book holds an approved sale of the schedule.
func scheduleSale(book openrails.DeclaredBilling, railSub string) bool {
	for _, t := range book.Transactions {
		if t.RailSubscriptionID == railSub && t.Success && (t.Type == "" || t.Type == "sale") {
			return true
		}
	}
	return false
}
