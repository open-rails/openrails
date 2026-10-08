//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

// bookTier is one catalog price of a legacy NMI book, linked to the NMI plan
// the legacy system created long ago.
type bookTier struct {
	price  *billing.Price
	ent    string
	plan   string
	amount string
	cents  int64
	days   int
}

func (w *world) bookTier(name string, cents int64, days int) bookTier {
	w.t.Helper()
	tier := bookTier{plan: "lb_" + name + "_" + uuid.NewString()[:8], amount: decimalCents(cents), cents: cents, days: days, ent: "content:" + name + "-" + uuid.NewString()[:6]}
	w.nmi.AddPlan(nmimock.Plan{ID: tier.plan, Name: "Legacy " + tier.plan, Amount: tier.amount, Days: days})
	client := w.client[embedded]
	product, err := client.CreateProduct(w.t.Context(), billing.CreateProductParams{Key: "lb-" + name + "-" + uuid.NewString()[:8], DisplayName: name, EntitlementsSpec: map[string]*int{tier.ent: nil}})
	require.NoError(w.t, err)
	hours := days * 24
	tier.price, err = client.CreatePrice(w.t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: cents * 10_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours, PSPLinks: map[string]map[string]string{"nmi": {"plan_id": tier.plan}}})
	require.NoError(w.t, err)
	return tier
}

// bookRow is one legacy subscription as the legacy system knows it.
type bookRow struct {
	source   string
	tier     bookTier
	c        *customer
	vault    string
	schedule string
	paid     time.Time
	cancel   billing.CancelEvidence
	dunning  *billing.DunningEvidence
	declared bool // the vault card is declared in the book
	months   int  // a calendar-month NMI schedule instead of the plan's days
}

// legacyBook accumulates rows into one DeclaredBilling.
type legacyBook struct {
	w    *world
	book billing.DeclaredBilling
	rows []*bookRow
}

func (w *world) newLegacyBook() *legacyBook {
	return &legacyBook{w: w, book: billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "nmi"}}}
}

// add creates the row at the fake NMI (vault, schedule, the sale that paid
// the current period) and declares it. c may be shared between rows; vault ""
// opens a new one.
func (b *legacyBook) add(r *bookRow) *bookRow {
	w := b.w
	t := w.t
	t.Helper()
	if r.c == nil {
		r.c = w.newCustomer()
	}
	customerID, err := billing.ParseCustomerID(r.c.id)
	require.NoError(t, err)
	priceID := r.tier.price.ID
	newVault := r.vault == ""
	if newVault {
		r.vault = w.nmi.AddVault(visa)
	}
	r.schedule = w.nmi.AddSchedule(nmimock.Schedule{Vault: r.vault, Plan: r.tier.plan, Amount: r.tier.amount, Days: r.tier.days, Months: r.months, NextBilling: r.paid})
	start := r.paid.AddDate(0, 0, -r.tier.days)
	if r.months > 0 {
		start = r.paid.AddDate(0, -r.months, 0)
	}
	sale := w.nmi.AddScheduleSale(r.schedule, start)
	method := &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: r.vault, RailMethodRef: w.nmi.Vault(r.vault).BillingID}
	if r.declared && newVault {
		b.book.PaymentMethods = append(b.book.PaymentMethods, billing.DeclaredPaymentMethod{Customer: customerID, Rail: "nmi", RailCustomerRef: r.vault, RailMethodRef: method.RailMethodRef, Card: declaredCard(visa)})
	}
	b.book.Customers = append(b.book.Customers, billing.DeclaredCustomer{Customer: customerID})
	paid := r.paid
	b.book.Subscriptions = append(b.book.Subscriptions, billing.DeclaredSubscription{SourceID: r.source, Customer: customerID, Price: priceID, Rail: "nmi", RailSubscriptionID: r.schedule,
		StartedAt: start.AddDate(-1, 0, 0), PaidThrough: &paid, Cancel: r.cancel, Dunning: r.dunning, PaymentMethod: method})
	b.book.Transactions = append(b.book.Transactions, billing.DeclaredTransaction{RailSubscriptionID: r.schedule, TransactionID: sale.TransactionID, Success: true, Amount: r.tier.cents * 10_000, Currency: "USD", OccurredAt: start})
	b.rows = append(b.rows, r)
	return r
}

// sub is the row's local subscription.
func (r *bookRow) sub(w *world, tp topology) *billing.Subscription {
	w.t.Helper()
	subs, err := w.client[tp].ListSubscriptions(w.t.Context(), billing.SubscriptionListParams{CustomerID: r.c.customerID()})
	require.NoError(w.t, err)
	for i := range subs.Items {
		if str(subs.Items[i].RailSubscriptionID) == r.schedule {
			return &subs.Items[i]
		}
	}
	w.t.Fatalf("%s: no local subscription for schedule %s", r.source, r.schedule)
	return nil
}

// nmiWrites is every provider mutation OpenRails sent to NMI.
func (w *world) nmiWrites() []providerCall {
	return calls(w.nmi.Calls())
}

type refreshMerchant struct {
	MerchantID uuid.UUID `json:"merchant_id"`
}

func (refreshMerchant) Kind() string { return "openrails.provider_refresh_merchant" }

// pull runs one scheduled Provider Refresh pass for the merchant (roster,
// transaction windows and the unknown-cohort probe) and waits for it.
func (w *world) pull() {
	w.t.Helper()
	w.refreshProviders()
}

// localCharges is the transaction ids of a customer's recorded captures.
func (w *world) localCharges(tp topology, customerID string) []string {
	var ids []string
	for _, p := range completed(w.payments(tp, customerID)) {
		ids = append(ids, p.TransactionID)
	}
	sort.Strings(ids)
	return ids
}

// nmiCharges is the transaction ids of NMI's approved sales on a vault.
func (w *world) nmiCharges(vault string) []string {
	var ids []string
	for _, s := range w.nmi.ledger(vault) {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

// A realistic legacy book lands in one ImportBilling call: every status the
// legacy system held, daily/monthly/calendar-monthly/yearly cadences, a
// customer with two memberships on one multi-card vault, an orphan card
// reference. Refunds in the book never become charges. Re-import changes
// nothing. The first enforcing pull finds the NMI-only schedule, mirrors the
// schedule NMI deleted, and raises no false drift. OpenRails never writes to
// NMI.
func TestLegacyNMIBookImport(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.waive("recorded", "the orphan-card row is blocked from import, so its legacy NMI charge has no local payment by design")
			now := w.clock.Now()
			daily, monthly, yearly := w.bookTier("daily", 199, 1), w.bookTier("monthly", 999, 30), w.bookTier("yearly", 9999, 365)
			writes := len(w.nmiWrites())

			b := w.newLegacyBook()
			active := b.add(&bookRow{source: "active", tier: monthly, paid: now.Add(10 * day), declared: true})
			shared := b.add(&bookRow{source: "daily", tier: daily, paid: now.Add(12 * time.Hour), declared: true})
			second := w.nmi.AddCard(shared.vault, mastercard)
			sharedID, err := billing.ParseCustomerID(shared.c.id)
			require.NoError(t, err)
			b.book.PaymentMethods = append(b.book.PaymentMethods, billing.DeclaredPaymentMethod{Customer: sharedID, Rail: "nmi", RailCustomerRef: shared.vault, RailMethodRef: second,
				Card: declaredCard(mastercard)})
			yearlyRow := b.add(&bookRow{source: "yearly", tier: yearly, c: shared.c, vault: shared.vault, paid: now.Add(200 * day)})
			last := now.Add(-12 * time.Hour)
			// Mid-dunning inside the grace window: NMI's failed renewal is
			// mirrored and standing access holds while NMI retries.
			pastDue := b.add(&bookRow{source: "past_due", tier: monthly, paid: now.Add(-day), declared: true,
				dunning: &billing.DunningEvidence{Retries: 1, LastRetryAt: &last, ScheduleLive: true}})
			w.nmi.EditSchedule(pastDue.schedule, func(s *nmimock.Schedule) { s.NextBilling = pastDue.paid.AddDate(0, 0, 30) })
			canceled := b.add(&bookRow{source: "canceled", tier: monthly, paid: now.Add(20 * day), declared: true,
				cancel: billing.CancelEvidence{Kind: "user_canceled", At: now.Add(-5 * day)}})
			w.nmi.DeleteSchedule(canceled.schedule)
			expired := b.add(&bookRow{source: "expired", tier: monthly, paid: now.Add(-35 * day), declared: true,
				cancel: billing.CancelEvidence{Kind: "provider_terminated", At: now.Add(-35 * day)}})
			w.nmi.DeleteSchedule(expired.schedule)
			// A paused NMI schedule bills nothing: it is declared as the
			// member's cancellation at the pause, with runway, and the paused
			// schedule is left alone (schedule_live=false).
			paused := b.add(&bookRow{source: "paused", tier: monthly, paid: now.Add(5 * day), declared: true,
				cancel: billing.CancelEvidence{Kind: "user_canceled", At: now.Add(-2 * day)}})
			w.nmi.EditSchedule(paused.schedule, func(s *nmimock.Schedule) { s.Paused = true })
			calendar := b.add(&bookRow{source: "calendar", tier: monthly, months: 1, paid: now.Add(15 * day), declared: true})
			gone := b.add(&bookRow{source: "gone_at_nmi", tier: monthly, paid: now.Add(8 * day), declared: true})
			orphan := b.add(&bookRow{source: "orphan_card", tier: monthly, paid: now.Add(9 * day)})

			// The legacy ledger also holds a partial refund of the active
			// member's charge, under its own transaction id.
			refund := "legacy-refund-" + uuid.NewString()[:8]
			b.book.Transactions = append(b.book.Transactions, billing.DeclaredTransaction{RailSubscriptionID: active.schedule, TransactionID: refund, Type: "refund", Success: true, Amount: 5_000_000, Currency: "USD", OccurredAt: now.Add(-2 * day)})

			result, err := w.client[tp].ImportBilling(t.Context(), b.book)
			require.NoError(t, err)
			var imported []string
			for _, r := range b.rows {
				if r != orphan {
					imported = append(imported, r.source)
				}
			}
			sort.Strings(imported)
			require.Equal(t, imported, result.Imported, "%+v", result)
			require.Equal(t, []string{orphan.source}, result.Blocked)
			require.Contains(t, result.Reasons[orphan.source], "payment_method", "an orphan card reference is reported, never dropped")
			w.settle()
			w.converge()

			type want struct {
				row      *bookRow
				status   string
				cancel   string
				entitled bool
			}
			wants := []want{
				{active, "active", "", true}, {shared, "active", "", true}, {yearlyRow, "active", "", true}, {calendar, "active", "", true}, {gone, "active", "", true},
				{pastDue, "past_due", "", true}, {canceled, "canceled", "user", true}, {expired, "canceled", "expired", false}, {paused, "canceled", "user", true},
			}
			state := map[string]string{}
			for _, x := range wants {
				sub := x.row.sub(w, tp)
				require.Equal(t, x.status, string(sub.Status), x.row.source)
				require.Equal(t, "nmi_schedule", sub.CollectionPolicy, x.row.source, "every NMI schedule is dunned by OpenRails")
				require.NotNil(t, sub.PaymentMethodID, x.row.source)
				require.NotNil(t, sub.CurrentPeriodEndsAt, x.row.source)
				require.True(t, x.row.paid.Equal(*sub.CurrentPeriodEndsAt), "%s: period ends at the declared paid-through (%s vs %s)", x.row.source, x.row.paid, sub.CurrentPeriodEndsAt)
				if x.cancel != "" {
					require.NotNil(t, sub.CancelType, x.row.source)
					require.Equal(t, x.cancel, *sub.CancelType, x.row.source)
				}
				if x.entitled {
					require.True(t, x.row.c.entitled(x.row.tier.ent), "%s keeps its paid period", x.row.source)
				}
				state[x.row.source] = fmt.Sprintf("%s|%s|%v", sub.Status, sub.CurrentPeriodEndsAt.UTC(), sub.CancelType != nil)
			}
			require.False(t, expired.c.entitled(monthly.ent), "an expired membership grants nothing")
			require.Equal(t, time.Duration(24)*time.Hour, shared.sub(w, tp).CurrentPeriodEndsAt.Sub(*shared.sub(w, tp).CurrentPeriodStartsAt), "the daily period is one day")
			require.Equal(t, []string{w.nmiCharges(active.vault)[0]}, w.localCharges(tp, active.c.id), "a refund in the book is never a charge")
			require.Contains(t, w.openFindings("pull.reversal.unlinked"), "nmi:"+refund, "the unlinked refund is surfaced")
			charges := map[string][]string{}
			for _, r := range b.rows {
				charges[r.c.id] = w.localCharges(tp, r.c.id)
			}

			// Re-import is a no-op.
			again, err := w.client[tp].ImportBilling(t.Context(), b.book)
			require.NoError(t, err)
			require.Equal(t, imported, again.Skipped)
			require.Empty(t, again.Imported)
			require.Equal(t, []string{orphan.source}, again.Blocked)
			w.settle()
			for _, x := range wants {
				sub := x.row.sub(w, tp)
				require.Equal(t, state[x.row.source], fmt.Sprintf("%s|%s|%v", sub.Status, sub.CurrentPeriodEndsAt.UTC(), sub.CancelType != nil), x.row.source)
			}
			for _, r := range b.rows {
				require.Equal(t, charges[r.c.id], w.localCharges(tp, r.c.id), "re-import records nothing new")
			}

			// A schedule NMI bills that the book never mentioned, and a
			// booked schedule NMI has since deleted.
			strayVault := w.nmi.AddVault(visa)
			stray := w.nmi.AddSchedule(nmimock.Schedule{Vault: strayVault, Plan: monthly.plan, Amount: monthly.amount, Days: 30, Months: 0, NextBilling: now.Add(5 * day)})
			w.nmi.DeleteSchedule(gone.schedule)
			w.armDestructive()
			w.pull()
			require.Contains(t, w.openFindings("pull.subscription.missing"), stray, "the NMI-only schedule is surfaced")
			require.Equal(t, billing.SubscriptionCanceled, gone.sub(w, tp).Status, "the schedule NMI deleted is mirrored")
			require.Empty(t, w.openFindings("pull.subscription.drift"), "matching schedules raise no drift")
			require.Empty(t, w.openFindings("pull.payment_method.mismatch"), "each card of a multi-card vault matches its own billing entry")
			require.Equal(t, billing.SubscriptionActive, active.sub(w, tp).Status)
			require.Equal(t, billing.SubscriptionCanceled, paused.sub(w, tp).Status, "the paused schedule stays a runway cancellation")

			require.Zero(t, len(w.nmi.Attempts()), "OpenRails never charges a provider-owned book")
			require.Len(t, w.nmiWrites(), writes, "import and pull never write to NMI")
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}
