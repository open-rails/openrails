//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/nmimock"
)

// legacy is one imported provider-owned membership: the provider owns the
// schedule and OpenRails mirrors it.
type legacy struct {
	w        *world
	rail     string
	tp       topology
	c        *customer
	price    *billing.Price
	railSub  string
	sub      billing.SubscriptionID
	ent      string
	railCust string
}

// importLegacy creates a provider subscription at the fake provider and
// lands it through ImportBilling, the documented legacy-book entry point.
func importLegacy(t *testing.T, w *world, rail string, tp topology, configure ...func(*billing.DeclaredBilling)) *legacy {
	t.Helper()
	l := &legacy{w: w, rail: rail, tp: tp, ent: "content:legacy", c: w.newCustomer()}
	client := w.client[tp]
	group := benefitGroup(l.ent)
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "legacy-" + uuid.NewString()[:8], DisplayName: "Legacy membership", TierGroup: &group, Entitlements: []string{l.ent}})
	require.NoError(t, err)
	hours := monthHours
	links := map[string]map[string]string{"stripe": {"price_id": "price_legacy_" + uuid.NewString()[:8]}}
	if rail == "nmi" {
		links = map[string]map[string]string{"nmi": {"plan_id": "legacy_plan_" + uuid.NewString()[:8]}}
	}
	l.price, err = client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 9_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours, PSPLinks: links})
	require.NoError(t, err)

	start := w.clock.Now().Add(-10 * day)
	end := start.Add(monthHours * time.Hour)
	customerID, err := billing.ParseCustomerID(l.c.id)
	require.NoError(t, err)
	priceID := l.price.ID
	book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: rail}, Customers: []billing.DeclaredCustomer{{Customer: customerID}}}
	switch rail {
	case "stripe":
		l.railCust = "cus_legacy" + uuid.NewString()[:8]
		method := "pm_legacy" + uuid.NewString()[:8]
		l.railSub = w.stripe.AddSubscription(l.railCust, method, links["stripe"]["price_id"], 999, start, end)
		book.PaymentMethods = []billing.DeclaredPaymentMethod{{Customer: customerID, Rail: "stripe", RailCustomerRef: l.railCust, RailMethodRef: method, Card: declaredCard(visa)}}
		book.Subscriptions = []billing.DeclaredSubscription{{SourceID: "legacy-" + l.railSub, Customer: customerID, Price: priceID, Rail: "stripe", RailSubscriptionID: l.railSub, StartedAt: start, PaidThrough: &end,
			PaymentMethod: &billing.PaymentMethodRef{Rail: "stripe", RailCustomerRef: l.railCust, RailMethodRef: method}}}
		book.Transactions = []billing.DeclaredTransaction{{RailSubscriptionID: l.railSub, TransactionID: w.stripe.LatestCharge(l.railSub), Success: true, Amount: 9_990_000, Currency: "USD", OccurredAt: start}}
	case "nmi":
		vault := w.nmi.AddVault(visa)
		l.railCust = vault
		l.railSub = w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: links["nmi"]["plan_id"], Amount: "9.99", NextBilling: end})
		book.PaymentMethods = []billing.DeclaredPaymentMethod{{Customer: customerID, Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.Vault(vault).BillingID, Card: declaredCard(visa)}}
		book.Subscriptions = []billing.DeclaredSubscription{{SourceID: "legacy-" + l.railSub, Customer: customerID, Price: priceID, Rail: "nmi", RailSubscriptionID: l.railSub, StartedAt: start, PaidThrough: &end,
			PaymentMethod: &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.Vault(vault).BillingID}}}
		book.Transactions = []billing.DeclaredTransaction{{RailSubscriptionID: l.railSub, TransactionID: w.nmi.AddSale(nmimock.Sale{OrderID: "legacy-order", Vault: vault, Amount: "9.99", At: start}).TransactionID, Success: true, Amount: 9_990_000, Currency: "USD", OccurredAt: start}}
	}
	for _, apply := range configure {
		apply(&book)
	}
	if rail == "nmi" {
		if book.Subscriptions[0].Dunning != nil {
			w.nmi.EditSchedule(l.railSub, func(s *nmimock.Schedule) {
				s.NextBilling = book.Subscriptions[0].PaidThrough.Add(monthHours * time.Hour)
			})
		}
	}
	result, err := client.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Len(t, result.Imported, 1, "%+v", result)
	w.settle()
	subs, err := client.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: l.c.customerID()})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	l.sub = subs.Items[0].ID
	sub := subs.Items[0]
	status := "active"
	if d := book.Subscriptions[0].Dunning; d != nil {
		status = "past_due"
		require.NotNil(t, sub.Dunning)
		require.Equal(t, d.Retries, sub.Dunning.Attempts)
		require.NotNil(t, w.graceEnds(sub.ID))
	}
	require.Equal(t, status, string(sub.Status))
	require.Equal(t, l.railSub, str(sub.RailSubscriptionID))
	wantPolicy := "provider"
	if rail == "nmi" {
		wantPolicy = "nmi_schedule" // every NMI schedule is dunned by OpenRails
	}
	require.Equal(t, wantPolicy, sub.CollectionPolicy)
	require.NotNil(t, sub.PaymentMethodID)
	charges := l.engineCharges()
	replay, err := client.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Equal(t, []string{book.Subscriptions[0].SourceID}, replay.Skipped)
	require.Equal(t, charges, l.engineCharges(), "import replay never charges")
	if rail == "nmi" && book.PaymentMethods[0].RecurringTransactionID != "" {
		conflict := book
		conflict.PaymentMethods = append([]billing.DeclaredPaymentMethod(nil), book.PaymentMethods...)
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

// engineCharges counts provider mutations OpenRails could use to charge.
func (l *legacy) engineCharges() int {
	if l.rail == "stripe" {
		return len(l.w.stripe.Mutations("/v1/payment_intents"))
	}
	return len(l.w.nmi.Attempts())
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
			require.Equal(t, http.StatusOK, w.deliver(rail, stripeEvent("customer.subscription.updated", w.stripe.DraftRenewal(l.railSub))))
			sub := w.subscription(tp, l.sub)
			require.Equal(t, billing.SubscriptionActive, sub.Status, "a draft invoice is not a failed renewal")
			for _, p := range w.payments(tp, l.c.id) {
				require.NotEqual(t, "failed", p.Status, "no failed payment for a draft invoice")
			}
			first = stripeEvent("invoice.paid", w.stripe.CollectDraft(l.railSub))
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
		inv := l.w.stripe.RenewSubscription(l.railSub, paid)
		kind := "invoice.paid"
		if !paid {
			kind = "invoice.payment_failed"
		}
		return stripeEvent(kind, inv)
	}
	sale := l.w.nmi.RenewSchedule(l.railSub, paid)
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
		stale := l.w.stripe.Subscription(l.railSub)
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
	res, err := hosttools.Converge(w.t.Context(), engine.Graph(w.rt), w.client[embedded].MerchantID())
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
			w.refreshProviders()
			_, err := w.client[tp].CancelSubscription(t.Context(), l.sub, billing.CancelSubscriptionParams{Reason: "member asked"})
			require.NoError(t, err)
			w.settle()
			w.advance(time.Hour)
			w.wake()
			require.NotNil(t, w.subscription(tp, l.sub).CanceledAt)
			if rail == "stripe" {
				require.Equal(t, true, w.stripe.Subscription(l.railSub)["cancel_at_period_end"], "Stripe stops renewing")
			} else {
				require.False(t, w.nmi.ScheduleLive(l.railSub), "the NMI schedule is deleted")
			}
			require.True(t, l.c.entitled(l.ent), "the paid period is kept")
		})
		t.Run("provider_cancel", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importLegacy(t, w, rail, tp)
			w.converge()
			w.refreshProviders()
			require.Equal(t, http.StatusOK, w.deliver(rail, l.providerCancelNotice()))
			sub := w.subscription(tp, l.sub)
			require.Equal(t, billing.SubscriptionCanceled, sub.Status, "the provider's own cancellation is mirrored")
		})
		t.Run("provider_payment_failed", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			l := importLegacy(t, w, rail, tp)
			w.converge()
			w.refreshProviders()
			w.advanceHealthyTo(l.periodEnd().Add(time.Hour))
			require.Equal(t, http.StatusOK, w.deliver(rail, l.providerRenewal(false)))
			sub := w.subscription(tp, l.sub)
			require.Equal(t, billing.SubscriptionPastDue, sub.Status, "the provider's failed renewal is mirrored")
			require.True(t, l.c.entitled(l.ent), "provider-owned dunning keeps standing access")
			owner := map[string]string{"stripe": "provider", "nmi": "nmi_schedule"}[rail]
			recorded := w.attempts(l.c.id)
			require.NotEmpty(t, recorded)
			rebill := recorded[len(recorded)-1]
			require.Equal(t, []string{"rebill", owner}, []string{rebill.Kind, rebill.Owner}, "the provider's decline is the cycle's rebill attempt")
			require.NotEqual(t, "approved", rebill.Category)
			charges := l.engineCharges()
			w.runRenewals()
			require.Equal(t, charges, l.engineCharges(), "OpenRails leaves the provider's dunning alone")
			// The host's account-deletion callback cancels what it finds.
			_, err := w.client[tp].CancelSubscription(t.Context(), l.sub, billing.CancelSubscriptionParams{Reason: "Account deletion evt_2", AccountDeletion: true})
			require.NoError(t, err)
			require.NotNil(t, w.subscription(tp, l.sub).CanceledAt)
			w.advance(time.Hour)
			w.wake()
			if rail == "stripe" {
				require.Equal(t, "canceled", w.stripe.Subscription(l.railSub)["status"], "a delinquent Stripe schedule ends now, so its open invoice stops retrying")
			} else {
				// Documented: the destructive-action switch ships off and holds
				// every NMI schedule delete until an operator arms it.
				require.True(t, w.nmi.ScheduleLive(l.railSub), "the delete waits for the operator's switch")
				w.armDestructive()
				w.advance(time.Hour)
				w.wake()
				require.False(t, w.nmi.ScheduleLive(l.railSub), "the held delete runs once armed")
			}
		})
	})
}

// providerCancelNotice ends the schedule at the provider and returns its notice.
func (l *legacy) providerCancelNotice() obj {
	if l.rail == "stripe" {
		return stripeEvent("customer.subscription.deleted", l.w.stripe.CancelSubscription(l.railSub))
	}
	l.w.nmi.DeleteSchedule(l.railSub)
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
			l := importLegacy(t, w, "nmi", tp, func(book *billing.DeclaredBilling) {
				declareRecurringAnchor(book)
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
			require.Equal(t, billing.SubscriptionPastDue, sub.Status)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(end), "a future schedule date cannot grant an unpaid period")
			require.NotNil(t, nextRetry(sub), "OpenRails schedules recovery after the provider decline")
			require.WithinDuration(t, end.Add(48*time.Hour), *nextRetry(sub), time.Second, "NMI never retries; OpenRails' first retry is the schedule's +2d from NMI's decline")
			require.Zero(t, len(w.nmi.Attempts()), "read-only posture holds automatic recovery")
			w.cfg = nil
			w.restart()
			w.advanceHealthyTo(nextRetry(sub).Add(time.Second))
			w.runRenewals()
			sub = w.subscription(tp, l.sub)
			require.Equal(t, billing.SubscriptionActive, sub.Status)
			require.Equal(t, "nmi_schedule", sub.CollectionPolicy)
			require.Equal(t, l.railSub, str(sub.RailSubscriptionID))
			require.True(t, sub.CurrentPeriodEndsAt.Equal(end.Add(monthHours*time.Hour)))
			require.Nil(t, nextRetry(sub))
			require.Equal(t, 1, len(w.nmi.Attempts()), "one automatic recovery charge")
			require.Len(t, w.nmi.ledger(""), 2, "initial and recovered periods each charged once")
			paid := completed(w.payments(tp, l.c.id))
			require.Len(t, paid, 2)
			recovery := w.nmi.LastSale()
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
			require.True(t, w.nmi.ScheduleLive(l.railSub), "NMI still owns the next scheduled charge")
			require.True(t, l.c.entitled(l.ent))
			w.runRenewals()
			require.Equal(t, 1, len(w.nmi.Attempts()), "a repeated due pass cannot recharge the recovered period")
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

// Import preserves the source recovery history as well as ownership. It
// must not submit a fresh charge when restoring or replaying this book.
func TestNMIProviderDunningImportRetainsRetryHistory(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	importLegacy(t, w, "nmi", remote, func(book *billing.DeclaredBilling) {
		last := w.clock.Now().Add(-2 * time.Hour)
		declareRecurringAnchor(book)
		book.Subscriptions[0].Dunning = &billing.DunningEvidence{Retries: 2, LastRetryAt: &last, ScheduleLive: true}
	})
	require.Zero(t, len(w.nmi.Attempts()))
}

// A stalled OpenRails-dunned schedule (past_due, no retry scheduled) resumes
// at the dunning schedule's next step after its last attempt, never at once.
// Past grace it is left to grace_exhausted, and provider-owned Stripe and NMI
// rows are never given a retry.
func TestDunningStallResumesOnSchedule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	dunning := func(book *billing.DeclaredBilling) { declareRecurringAnchor(book) }
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
		_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.subscriptions
			SET lifecycle_rev = lifecycle_rev + 1, status = 'past_due', next_retry_at = NULL, retry_attempts = 2, last_retry_at = $2, grace_ends_at = $3
			WHERE id = $1`), l.sub.UUID(), last, grace)
		require.NoError(t, err)
	}
	stall(stalled, now.Add(10*day))
	stall(lapsed, now.Add(-time.Minute))
	stall(stripeOwned, now.Add(10*day))
	stall(nmiOwned, now.Add(10*day))

	w.converge()
	sub := w.subscription(embedded, stalled.sub)
	require.Equal(t, billing.SubscriptionPastDue, sub.Status)
	require.NotNil(t, nextRetry(sub), "the stalled schedule resumes")
	require.WithinDuration(t, last.Add(3*day), *nextRetry(sub), time.Second, "third attempt: +5d after the first failure, 3d after the second")
	sub = w.subscription(embedded, lapsed.sub)
	require.Nil(t, nextRetry(sub), "past grace, grace_exhausted owns it")
	require.Equal(t, billing.SubscriptionUnverified, sub.Status)
	sub = w.subscription(embedded, stripeOwned.sub)
	require.Nil(t, nextRetry(sub), "stripe: the provider owns its retries")
	require.Equal(t, billing.SubscriptionPastDue, sub.Status)
	sub = w.subscription(embedded, nmiOwned.sub)
	require.NotNil(t, nextRetry(sub), "nmi: NMI never retries, so OpenRails dunning resumes")
	require.Equal(t, billing.SubscriptionPastDue, sub.Status)
	w.runRenewals()
	require.Zero(t, len(w.nmi.Attempts()))
	require.Equal(t, charges, stripeOwned.engineCharges())
}

// A lapsed NMI schedule whose date has not moved on is never charged by
// OpenRails, even once the rebill watch records the cycle missed: NMI may
// still bill it. When NMI's renewal arrives late, the period is paid exactly
// once.
func TestNMIProviderDunningLapseWithoutDeclineNeverCharged(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded, func(book *billing.DeclaredBilling) {
		declareRecurringAnchor(book)
	})
	w.converge()
	end := l.periodEnd()
	for _, step := range []time.Duration{end.Sub(w.clock.Now()) + time.Hour, 49 * time.Hour} {
		w.advance(step)
		w.converge()
		w.watchRebills()
		w.runRenewals()
		sub := w.subscription(embedded, l.sub)
		require.NotEqual(t, billing.SubscriptionPastDue, sub.Status, "a lapse is not a decline")
		require.Nil(t, nextRetry(sub))
		require.Zero(t, len(w.nmi.Attempts()), "OpenRails never charges without a seen decline")
	}
	require.Equal(t, "provider_stalled", w.missReason(l.sub, end))

	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true)))
	w.runRenewals()
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status)
	require.True(t, sub.CurrentPeriodEndsAt.After(end))
	require.Zero(t, len(w.nmi.Attempts()))
	require.Len(t, w.nmi.ledger(""), 2, "the initial and NMI's renewal, nothing more")
	require.Len(t, completed(w.payments(embedded, l.c.id)), 2)
}

// declareRecurringAnchor declares the NMI card's recurring agreement: the
// schedule's signup sale.
func declareRecurringAnchor(book *billing.DeclaredBilling) {
	book.PaymentMethods[0].RecurringTransactionID = book.Transactions[0].TransactionID
}
