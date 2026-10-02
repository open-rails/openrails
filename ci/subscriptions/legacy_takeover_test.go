//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/nmimock"
)

const takeoverDrift = "life.engine_takeover.schedule_drift"

// importTakeoverLegacy is a provider-owned legacy NMI membership whose vaulted
// card carries the verified recurring agreement a takeover charges under.
func importTakeoverLegacy(t *testing.T, w *world, tp topology) *legacy {
	t.Helper()
	return importLegacy(t, w, "nmi", tp, func(book *openrails.DeclaredBilling) {
		book.PaymentMethods[0].RecurringTransactionID = book.Transactions[0].TransactionID
	})
}

func (l *legacy) takeover(key string) *openrails.EngineTakeover {
	l.w.t.Helper()
	out, err := l.w.client[l.tp].TakeOverBilling(l.w.t.Context(), l.sub, key)
	require.NoError(l.w.t, err)
	l.w.settle()
	return out
}

func (l *legacy) takeoverState() *openrails.EngineTakeover {
	l.w.t.Helper()
	out, err := l.w.client[l.tp].GetEngineTakeover(l.w.t.Context(), l.sub)
	require.NoError(l.w.t, err)
	return out
}

// Taking over a legacy NMI membership deletes its NMI schedule exactly once,
// before NMI could bill the next period; the legacy row ends with its paid
// access and an engine successor charges the same vaulted card, under the
// imported recurring agreement, exactly once per period from the boundary.
func TestNMIEngineTakeover(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importTakeoverLegacy(t, w, tp)
			w.converge()
			end := l.periodEnd()
			client := w.client[tp]

			preview, err := client.PreviewEngineTakeover(t.Context(), l.sub)
			require.NoError(t, err)
			require.Equal(t, "ready", preview.Stage)
			require.True(t, preview.Anchor.Equal(end))
			require.EqualValues(t, 9_990_000, preview.Amount)
			require.Zero(t, w.nmi.ScheduleDeletes(l.railSub), "a preview changes nothing")

			key := "takeover-" + uuid.NewString()
			done := l.takeover(key)
			require.Equal(t, "completed", done.Stage, "%+v", done)
			require.NotNil(t, done.SuccessorSubscriptionID)
			require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub), "the NMI schedule is deleted exactly once")
			require.False(t, w.nmi.ScheduleLive(l.railSub))
			again := l.takeover(key)
			require.Equal(t, done.ID, again.ID, "the key replays the operation")
			require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub))

			old := w.subscription(tp, l.sub)
			require.Equal(t, "cancelled", old.Status)
			require.Equal(t, "engine_takeover", *old.CancelType)
			require.True(t, old.CurrentPeriodEndsAt.Equal(end), "the legacy paid period is kept")
			successor := w.subscription(tp, *done.SuccessorSubscriptionID)
			require.Equal(t, "engine", successor.CollectionPolicy)
			require.Equal(t, "active", successor.Status)
			require.Empty(t, successor.RailSubscriptionID)
			require.True(t, successor.CurrentPeriodEndsAt.Equal(end))
			require.Equal(t, l.price.ID, successor.PriceID)
			require.True(t, l.c.entitled(l.ent))

			// Nothing is charged before the boundary.
			w.advance(end.Sub(w.clock.Now()) - 2*time.Hour)
			w.runRenewals()
			require.Zero(t, len(w.nmi.Attempts()))
			require.True(t, l.c.entitled(l.ent), "no gap before the boundary")

			// At the boundary the engine charges once, on the legacy card and agreement.
			w.advance(3 * time.Hour)
			w.runRenewals()
			w.until(func() bool { return w.subscription(tp, successor.ID).CurrentPeriodEndsAt.After(end) }, "the first engine renewal")
			w.runRenewals()
			require.Equal(t, 1, len(w.nmi.Attempts()), "exactly one engine charge for the period")
			sale := w.nmi.LastSale()
			require.Equal(t, "9.99", sale.Amount)
			require.Equal(t, l.railCust, sale.Vault)
			require.Equal(t, "merchant", sale.InitiatedBy)
			require.Equal(t, "used", sale.Indicator)
			require.Equal(t, w.nmi.ledger("")[0].ID, sale.Initial, "the imported recurring agreement")
			require.Empty(t, sale.ScheduleID, "NMI's schedule never billed")
			require.True(t, l.c.entitled(l.ent), "access continues across the boundary")
			require.Len(t, completed(w.payments(tp, l.c.id)), 2, "legacy period and first engine period")
			w.converge()
			require.Empty(t, w.openFindings(duplicateCharge))
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

// Every refusal is typed, identical on both topologies, and leaves NMI
// billing untouched.
func TestNMIEngineTakeoverRefusals(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp)+"/no_recurring_agreement", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			// The book names no recurring agreement and no sale of the schedule,
			// so there is no anchor to take over (or to dun) with.
			w.waive("recorded", "the book's only sale is declared without its schedule, so it is not attributed to the membership")
			l := importLegacy(t, w, "nmi", tp, func(book *openrails.DeclaredBilling) {
				book.Transactions[0].RailSubscriptionID = ""
			})
			require.Contains(t, w.openFindings("life.import.no_recurring_anchor"), l.railSub)
			_, err := w.client[tp].TakeOverBilling(t.Context(), l.sub, "k-"+uuid.NewString())
			requireCode(t, err, http.StatusConflict, openrails.CodeEngineTakeoverNoAgreement)
			_, err = w.client[tp].GetEngineTakeover(t.Context(), l.sub)
			requireCode(t, err, http.StatusNotFound, openrails.CodeEngineTakeoverNotFound)
			require.Zero(t, w.nmi.ScheduleDeletes(l.railSub))
		})
		t.Run(string(tp)+"/boundary_too_close", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importTakeoverLegacy(t, w, tp)
			w.advance(l.periodEnd().Sub(w.clock.Now()) - 23*time.Hour)
			_, err := w.client[tp].PreviewEngineTakeover(t.Context(), l.sub)
			requireCode(t, err, http.StatusConflict, openrails.CodeEngineTakeoverBoundaryTooClose)
			_, err = w.client[tp].TakeOverBilling(t.Context(), l.sub, "k-"+uuid.NewString())
			requireCode(t, err, http.StatusConflict, openrails.CodeEngineTakeoverBoundaryTooClose)
			require.Zero(t, w.nmi.ScheduleDeletes(l.railSub))
			require.Equal(t, "nmi_schedule", w.subscription(tp, l.sub).CollectionPolicy)
		})
		t.Run(string(tp)+"/abandon_after_delete", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importTakeoverLegacy(t, w, tp)
			require.Equal(t, "completed", l.takeover("k-"+uuid.NewString()).Stage)
			_, err := w.client[tp].AbandonEngineTakeover(t.Context(), l.sub)
			requireCode(t, err, http.StatusConflict, openrails.CodeEngineTakeoverCommitted)
			require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub))
		})
	}
}

// A schedule that drifted at NMI (amount, billing date, vault) is never
// deleted: the takeover ends not executed with a drift finding.
func TestNMIEngineTakeoverScheduleDrift(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(w *world, s *nmimock.Schedule)
	}{
		{"amount", func(_ *world, s *nmimock.Schedule) { s.Amount = "14.99" }},
		{"billing_date", func(_ *world, s *nmimock.Schedule) { s.NextBilling = s.NextBilling.AddDate(0, 0, -3) }},
		{"vault", func(w *world, s *nmimock.Schedule) { s.Vault = w.nmi.AddVault(mastercard) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importTakeoverLegacy(t, w, remote)
			s := w.nmi.Schedule(l.railSub)
			tc.edit(w, &s)
			w.nmi.EditSchedule(l.railSub, func(live *nmimock.Schedule) { *live = s })
			out := l.takeover("k-" + uuid.NewString())
			require.Equal(t, "not_executed", out.Stage, "%+v", out)
			require.Equal(t, "failed_terminal", out.Status)
			require.Zero(t, w.nmi.ScheduleDeletes(l.railSub), "a drifted schedule is never deleted")
			require.True(t, w.nmi.ScheduleLive(l.railSub))
			require.Contains(t, w.openFindings(takeoverDrift), l.sub.UUID().String())
			sub := w.subscription(remote, l.sub)
			require.Equal(t, "active", sub.Status)
			require.Equal(t, "nmi_schedule", sub.CollectionPolicy)
		})
	}
}

// With the destructive switch disarmed the takeover is held and NMI keeps
// billing; an abandoned held takeover never runs; a held takeover that
// passes its cutoff ends not executed, and NMI's own renewal stays mirrored.
func TestNMIEngineTakeoverHeld(t *testing.T) {
	t.Parallel()
	t.Run("cutoff_passes", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importTakeoverLegacy(t, w, embedded)
		w.converge()
		held := l.takeover("k-" + uuid.NewString())
		require.Equal(t, "pending", held.Status, "%+v", held)
		require.Equal(t, "held", held.Stage)
		require.Zero(t, w.nmi.ScheduleDeletes(l.railSub))

		end := l.periodEnd()
		w.advance(end.Sub(w.clock.Now()) + time.Hour)
		require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true)))
		require.True(t, l.periodEnd().After(end), "NMI's renewal is mirrored while the takeover is held")
		require.True(t, l.c.entitled(l.ent))

		w.armDestructive()
		w.until(func() bool { return l.takeoverState().Stage == "not_executed" }, "the held takeover passes its cutoff")
		require.Zero(t, w.nmi.ScheduleDeletes(l.railSub))
		require.True(t, w.nmi.ScheduleLive(l.railSub))
		require.Zero(t, len(w.nmi.Attempts()), "OpenRails never charged")
		require.Equal(t, "nmi_schedule", w.subscription(embedded, l.sub).CollectionPolicy)
	})
	t.Run("abandon", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importTakeoverLegacy(t, w, remote)
		require.Equal(t, "held", l.takeover("k-"+uuid.NewString()).Stage)
		abandoned, err := w.client[remote].AbandonEngineTakeover(t.Context(), l.sub)
		require.NoError(t, err)
		require.Equal(t, "abandoned", abandoned.Stage)
		w.armDestructive()
		w.advance(time.Hour)
		w.wake()
		require.Zero(t, w.nmi.ScheduleDeletes(l.railSub), "an abandoned takeover never runs")
		require.Equal(t, "abandoned", l.takeoverState().Stage)
		// A fresh takeover is admissible again.
		require.Equal(t, "completed", l.takeover("k-"+uuid.NewString()).Stage)
		require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub))
	})
}

// A process that dies with the schedule delete in flight (the delete
// reached NMI, its answer was lost) resolves by reading NMI: the takeover
// completes with exactly one delete.
func TestNMIEngineTakeoverCrashDuringDelete(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importTakeoverLegacy(t, w, embedded)
	g := w.nmi.hold(newGate(func(r *http.Request) bool {
		return r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/v5/subscriptions/")
	}, true))
	batch, err := w.client[embedded].TakeOverBillingBatch(t.Context(), openrails.EngineTakeoverBatchRequest{MaxSubscriptions: 1})
	require.NoError(t, err)
	require.Len(t, batch.Admitted, 1)
	promoteCtx, stopPromoting := context.WithCancel(t.Context())
	promoterDone := make(chan struct{})
	go func() {
		defer close(promoterDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-promoteCtx.Done():
				return
			case <-ticker.C:
				w.settleQuiet()
			}
		}
	}()
	select {
	case <-g.arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("the delete never reached NMI")
	}
	stopPromoting()
	<-promoterDone
	w.stop()
	w.nmi.unhold()
	w.start()
	w.advance(30 * time.Minute)
	w.wake()
	w.until(func() bool { return l.takeoverState().Stage == "completed" }, "the takeover resolves from NMI's record")
	require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub), "exactly one delete")
	require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status)
	require.Zero(t, len(w.nmi.Attempts()))
}

// importLegacyBook lands n legacy NMI memberships on one price in one book.
func importLegacyBook(t *testing.T, w *world, n int) []*legacy {
	t.Helper()
	client := w.client[embedded]
	ent := "content:legacy-book"
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "book-" + uuid.NewString()[:8], DisplayName: "Legacy book", EntitlementsSpec: map[string]*int{ent: nil}})
	require.NoError(t, err)
	hours := monthHours
	plan := "legacy_plan_" + uuid.NewString()[:8]
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
		PSPLinks: map[string]map[string]string{"nmi": {"plan_id": plan}}})
	require.NoError(t, err)
	priceID, err := openrails.ParsePriceID(price.ID)
	require.NoError(t, err)
	book := openrails.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: openrails.PSPRef{Key: "nmi"}}
	var out []*legacy
	for i := range n {
		c := w.newCustomer()
		customerID, err := openrails.ParseCustomerID(c.id)
		require.NoError(t, err)
		start := w.clock.Now().Add(-10*day + time.Duration(i)*time.Minute)
		end := start.Add(monthHours * time.Hour)
		vault := w.nmi.AddVault(visa)
		railSub := w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: plan, Amount: "9.99", NextBilling: end})
		tx := w.nmi.AddSale(nmimock.Sale{OrderID: "legacy-order", Vault: vault, Amount: "9.99", At: start}).TransactionID
		ref := openrails.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.Vault(vault).BillingID}
		book.Customers = append(book.Customers, openrails.DeclaredCustomer{Customer: customerID})
		book.PaymentMethods = append(book.PaymentMethods, openrails.DeclaredPaymentMethod{Customer: customerID, Rail: "nmi", RailCustomerRef: vault, RailMethodRef: ref.RailMethodRef,
			InitialTransactionID: tx, RecurringTransactionID: tx, LastFour: "4242", CardType: "visa", ExpiryDate: "12/35"})
		book.Subscriptions = append(book.Subscriptions, openrails.DeclaredSubscription{SourceID: fmt.Sprintf("book-%d-%s", i, railSub), Customer: customerID, Price: priceID,
			Rail: "nmi", RailSubscriptionID: railSub, StartedAt: start, PaidThrough: &end, PaymentMethod: &ref})
		book.Transactions = append(book.Transactions, openrails.DeclaredTransaction{RailSubscriptionID: railSub, TransactionID: tx, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: start})
		out = append(out, &legacy{w: w, rail: "nmi", tp: embedded, c: c, railSub: railSub, railCust: vault, ent: ent})
	}
	result, err := client.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Len(t, result.Imported, n, "%+v", result)
	w.settle()
	for _, l := range out {
		subs, err := client.ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: l.c.id})
		require.NoError(t, err)
		require.Len(t, subs.Data, 1)
		l.sub = subs.Data[0].ID
	}
	return out
}

// A bulk takeover is admitted in one call and drained by the executor under
// the destructive volume breaker: at most the day's budget of NMI schedules
// is deleted, the rest stay held (and NMI-billed) behind an operator finding,
// each executed takeover deletes its schedule exactly once and nothing is
// charged. A repeated bulk call admits nothing twice.
func TestNMIEngineTakeoverBulk(t *testing.T) {
	t.Parallel()
	const n, budget = 30, 25
	w := newWorld(t)
	w.armDestructive()
	book := importLegacyBook(t, w, n)
	_, err := w.client[remote].TakeOverBillingBatch(t.Context(), openrails.EngineTakeoverBatchRequest{MaxSubscriptions: 1000})
	requireCode(t, err, http.StatusBadRequest, "invalid_param")
	batch, err := w.client[remote].TakeOverBillingBatch(t.Context(), openrails.EngineTakeoverBatchRequest{MaxSubscriptions: n})
	require.NoError(t, err)
	require.Len(t, batch.Admitted, n, "%+v", batch.Refused)
	w.settle()
	w.advance(time.Hour)
	w.wake()

	deleted, held := 0, 0
	for _, l := range book {
		deletes := w.nmi.ScheduleDeletes(l.railSub)
		require.LessOrEqual(t, deletes, 1, "no schedule is deleted twice")
		sub := w.subscription(embedded, l.sub)
		if deletes == 1 {
			deleted++
			require.Equal(t, "cancelled", sub.Status)
			require.Equal(t, "completed", l.takeoverState().Stage)
		} else {
			held++
			require.Equal(t, "active", sub.Status)
			require.True(t, w.nmi.ScheduleLive(l.railSub), "a held takeover leaves NMI billing")
			require.Equal(t, "held", l.takeoverState().Stage)
		}
	}
	require.Equal(t, budget, deleted, "the breaker caps the day's deletes exactly, whatever the executor concurrency")
	require.Positive(t, held)
	require.NotEmpty(t, w.openFindings("life.provider_intent.held_bulk"))
	require.Zero(t, len(w.nmi.Attempts()))

	again, err := w.client[embedded].TakeOverBillingBatch(t.Context(), openrails.EngineTakeoverBatchRequest{MaxSubscriptions: n})
	require.NoError(t, err)
	require.Empty(t, again.Admitted, "in-flight and completed takeovers are not admitted again")
	w.settle()
	for _, l := range book {
		require.LessOrEqual(t, w.nmi.ScheduleDeletes(l.railSub), 1)
	}
	require.Empty(t, w.nmi.Unexpected())
}
