//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/nmimock"
)

// liveMember declares a live NMI member as a legacy book does, without
// accepted schedule terms, and answers its schedule and vault.
func (w *world) liveMember(book *billing.DeclaredBilling, tier bookTier, schedule nmimock.Schedule, started, paid time.Time) (*customer, string, string) {
	w.t.Helper()
	c := w.newCustomer()
	schedule.Vault = w.nmi.AddVault(visa)
	id := w.nmi.AddSchedule(schedule)
	method := &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: schedule.Vault, RailMethodRef: w.nmi.Vault(schedule.Vault).BillingID}
	book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: c.customerID()})
	book.PaymentMethods = append(book.PaymentMethods, billing.DeclaredPaymentMethod{Customer: c.customerID(), Rail: "nmi", RailCustomerRef: schedule.Vault, RailMethodRef: method.RailMethodRef, Card: declaredCard(visa)})
	book.Subscriptions = append(book.Subscriptions, billing.DeclaredSubscription{SourceID: "live-" + id, Customer: c.customerID(), Price: tier.price.ID, Rail: "nmi",
		RailSubscriptionID: id, StartedAt: started, PaidThrough: &paid, PaymentMethod: method})
	return c, id, schedule.Vault
}

// scheduleFinding is the evidence of a subscription's schedule finding.
func (w *world) scheduleFinding(sub billing.Subscription) (status string, evidence map[string]any) {
	w.t.Helper()
	var raw []byte
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT status, evidence FROM billing.reconciliation_findings
		WHERE finding_type = 'life.import.schedule_unresolved' AND subject_key = $1`), "subscription:"+sub.ID.UUID().String()).Scan(&status, &raw))
	require.NoError(w.t, json.Unmarshal(raw, &evidence))
	return status, evidence
}

// A live NMI member whose book proves no schedule (its only declared charge
// is the signup, a cycle before the paid one) imports unresolved. The next
// provider refresh reads NMI's recurring record and accepts its terms: the
// signup, read again from NMI, and NMI's undeclared renewal settle as
// history, and NMI's next renewal binds the first accepted cycle.
func TestLiveNMIScheduleFromProviderRecord(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := t.Context()
	monthly := w.bookTier("record", 999, 30)
	now := w.clock.Now()
	paid := now.Add(10 * day)
	cycle, started := paid.Add(-30*day), paid.Add(-60*day)

	book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}}
	schedule := nmimock.Schedule{Plan: monthly.plan, Amount: monthly.amount, Days: 30, NextBilling: paid}
	member, scheduleID, vault := w.liveMember(&book, monthly, schedule, started, paid)
	signup := w.nmi.AddSale(nmimock.Sale{Vault: vault, Amount: monthly.amount, OrderID: "legacy-signup", At: started}).TransactionID
	renewal := w.nmi.AddScheduleSale(scheduleID, cycle).TransactionID // NMI's; the host's logs never saw it
	book.Transactions = append(book.Transactions, billing.DeclaredTransaction{RailSubscriptionID: scheduleID, TransactionID: signup, Success: true,
		Amount: 9_990_000, Currency: "USD", OccurredAt: started})

	res, err := w.client[remote].ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Equal(t, []string{"live-" + scheduleID}, res.Imported, "%+v", res)
	sub := w.subscriptionOf(member)
	require.Equal(t, billing.SubscriptionActive, sub.Status)
	require.Equal(t, []string{"subscription:" + sub.ID.UUID().String()}, w.openFindings("life.import.schedule_unresolved"))
	require.Equal(t, []string{"receipt:" + signup}, w.openFindings("pull.bill.unresolved"))
	require.Empty(t, w.bills(member.id), "no payment while the schedule is unproven")
	require.Zero(t, w.countRows(`SELECT count(*) FROM billing.provider_schedules`))

	// The refresh reads NMI's recurring record: its terms are the schedule.
	w.refreshProviders()
	var source string
	var firstDue time.Time
	var hours, months int
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT source_reference, first_due_at, cadence_hours, cadence_months FROM billing.provider_schedules WHERE subscription_id = $1`),
		sub.ID.UUID()).Scan(&source, &firstDue, &hours, &months))
	require.Equal(t, "nmi_subscription:"+scheduleID, source)
	require.True(t, firstDue.Equal(paid.UTC().Truncate(24*time.Hour)), "anchored on NMI's next charge date: %s", firstDue)
	require.Equal(t, []int{720, 0}, []int{hours, months})
	require.Empty(t, w.openFindings("life.import.schedule_unresolved"))
	require.Empty(t, w.openFindings("pull.bill.unresolved"))
	status, evidence := w.scheduleFinding(sub)
	require.Equal(t, "auto_fixed", status)
	resolution := evidence["resolution"].(map[string]any)
	require.Equal(t, source, resolution["schedule_source_reference"])
	provider := resolution["provider"].(map[string]any)
	require.Equal(t, "nmi_subscription", provider["source"])
	require.Equal(t, monthly.amount, provider["amount"])
	require.EqualValues(t, 30, provider["interval_days"])

	price := monthly.price.ID.String()
	history := func(at time.Time, kind string) billRow {
		end := at.Add(30 * day)
		return billRow{origin: "subscription", owner: "nmi_schedule", status: "complete", kind: kind, source: "provider_pre_schedule", total: 9_990_000, paid: 9_990_000, unit: 9_990_000,
			periodStart: &at, periodEnd: &end, purchasedAt: &at, providerAmount: true, price: price}
	}
	sameBill := func(want, got billRow) {
		t.Helper()
		require.True(t, want.periodStart.Equal(*got.periodStart) && want.periodEnd.Equal(*got.periodEnd) && want.purchasedAt.Equal(*got.purchasedAt), "%+v", got)
		want.periodStart, want.periodEnd, want.purchasedAt = got.periodStart, got.periodEnd, got.purchasedAt
		require.Equal(t, want, got)
	}
	bills := w.bills(member.id)
	require.Len(t, bills, 2, "%v", bills)
	sameBill(history(started, "initial"), bills[signup])
	sameBill(history(cycle, "renewal"), bills[renewal])
	require.Zero(t, w.countRows(`SELECT count(*) FROM billing.provider_cycle_receipts`), "history binds no cycle")

	// A re-import keeps the accepted schedule and reopens nothing.
	again, err := w.client[embedded].ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Equal(t, []string{"live-" + scheduleID}, again.Skipped, "%+v", again)
	require.Empty(t, w.openFindings("life.import.schedule_unresolved"))
	require.Empty(t, w.openFindings("pull.bill.unresolved"))
	require.Equal(t, 1, w.countRows(`SELECT count(*) FROM billing.provider_schedules`))
	require.Equal(t, bills, w.bills(member.id))

	// NMI renews on its date: the receipt pays the first accepted cycle.
	w.advance(paid.Sub(w.clock.Now()) + time.Hour)
	next := w.nmi.RenewSchedule(scheduleID, true).TransactionID
	w.refreshProviders()
	bills = w.bills(member.id)
	require.Len(t, bills, 3, "%v", bills)
	bound := bills[next]
	require.Equal(t, "provider_cycle", bound.source)
	require.Equal(t, "renewal", bound.kind)
	require.True(t, bound.periodStart.Equal(firstDue) && bound.periodEnd.Equal(firstDue.Add(30*day)), "%s..%s", bound.periodStart, bound.periodEnd)
	var due time.Time
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT due_at FROM billing.provider_cycle_receipts WHERE transaction_id = $1 AND schedule_source_reference = $2`), next, source).Scan(&due))
	require.True(t, due.Equal(firstDue))
	require.Empty(t, w.openFindings("pull.bill.unresolved"))
	require.Zero(t, len(w.nmi.Attempts()), "OpenRails charged nothing")
}

// NMI's record decides only when it agrees with the subscription's price: an
// amount or a frequency it does not bill, or no record at all, keeps the
// schedule unresolved with NMI's evidence, and no schedule is guessed.
func TestLiveNMIScheduleDisagreementStaysUnresolved(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := t.Context()
	monthly := w.bookTier("disagree", 999, 30)
	now := w.clock.Now()
	paid, started := now.Add(10*day), now.Add(-100*day)

	book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}}
	cheaper, _, _ := w.liveMember(&book, monthly, nmimock.Schedule{Plan: monthly.plan, Amount: "4.99", Days: 30, NextBilling: paid}, started, paid)
	weekly, _, _ := w.liveMember(&book, monthly, nmimock.Schedule{Plan: monthly.plan, Amount: monthly.amount, Days: 7, NextBilling: paid}, started, paid)
	gone, goneID, _ := w.liveMember(&book, monthly, nmimock.Schedule{Plan: monthly.plan, Amount: monthly.amount, Days: 30, NextBilling: paid}, started, paid)
	w.nmi.DeleteSchedule(goneID)

	res, err := w.client[embedded].ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Len(t, res.Imported, 3, "%+v", res)
	require.Len(t, w.openFindings("life.import.schedule_unresolved"), 3)

	for range 2 { // a second read changes nothing
		require.NoError(t, w.cliPullAs(config.ProviderWriteModeFull, "nmi", true, false))
		w.settle()
		require.Zero(t, w.countRows(`SELECT count(*) FROM billing.provider_schedules`), "no schedule is guessed")
		require.Len(t, w.openFindings("life.import.schedule_unresolved"), 3)

		status, evidence := w.scheduleFinding(w.subscriptionOf(cheaper))
		require.Equal(t, "requires_review", status)
		require.NotEmpty(t, evidence["reason"])
		require.Equal(t, "4.99", evidence["provider"].(map[string]any)["amount"])

		_, evidence = w.scheduleFinding(w.subscriptionOf(weekly))
		require.NotEmpty(t, evidence["reason"])
		require.EqualValues(t, 7, evidence["provider"].(map[string]any)["interval_days"])

		_, evidence = w.scheduleFinding(w.subscriptionOf(gone))
		require.NotEmpty(t, evidence["reason"])
		require.NotContains(t, evidence, "provider")
	}
	require.Empty(t, w.bills(cheaper.id))
	require.Zero(t, w.countRows(`SELECT count(*) FROM billing.payments WHERE customer_id = ANY($1)`,
		[]uuid.UUID{uuid.MustParse(cheaper.id), uuid.MustParse(weekly.id), uuid.MustParse(gone.id)}))
}
