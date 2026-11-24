//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

type usageDueJob struct{}

func (usageDueJob) Kind() string { return "openrails.usage_orders" }

func runUsageDue(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	for range 2 {
		job, err := fleet.Insert(t.Context(), usageDueJob{}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			row, err := fleet.JobGet(t.Context(), job.Job.ID)
			return err == nil && row.State == rivertype.JobStateCompleted
		}, 15*time.Second, 20*time.Millisecond)
	}
}

func usageBillingWorld(t *testing.T, enabled, manual bool, configure ...func(*world)) (*authorityWorld, *nmiJournal, billing.CustomerID) {
	t.Helper()
	a, p, customer := usageBillingAccount(t, enabled, manual, configure...)
	usageMeter(t, a.alpha, 10_000_000)
	return a, p, customer
}

// usageBillingAccount is alice with a saved card and a $30 USD credit line,
// billed on the 25th.
func usageBillingAccount(t *testing.T, enabled, manual bool, configure ...func(*world)) (*authorityWorld, *nmiJournal, billing.CustomerID) {
	t.Helper()
	changes := append([]func(*world){func(w *world) {
		w.clock.Advance(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC).Sub(w.clock.Now()))
		w.cfg.UsageBillingEnabled = enabled
		m := cfgMerchant(w.cfg, "alpha")
		m.UsageBilling = &billing.UsageBillingSettings{BillingDay: 25, Currencies: []billing.UsageBillingCurrency{{Currency: "USD", CreditLimit: 30_000_000, MinimumInvoiceAmount: 1_000_000, DelinquencyFloor: ptr(int64(0))}}}
		m.ArrearsGraceDays = ptr(0)
		setCfgMerchant(&w.cfg, "alpha", m)
	}}, configure...)
	a, p := newNMIWorld(t, changes...)
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: p.core.Tokenize(nmimock.Card{Last4: "4242", Exp: "1235"})})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var saved billing.PaymentMethod
	require.NoError(t, json.Unmarshal(raw, &saved))
	status, raw = a.request(t, http.MethodPut, "/v1/me/default-payment-methods/USD", a.aliceToken, billing.SetDefaultPaymentMethodParams{PaymentMethodID: saved.ID})
	require.Equal(t, http.StatusOK, status, string(raw))
	if manual {
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{InvoiceProfile: catalog.Value(billing.InvoiceProfile{CollectionMethod: billing.CollectSendInvoice})})
		require.NoError(t, err)
	}
	return a, p, customer
}

// postUsageMonth ends the month the usage occurred: it posts, and what it
// leaves owed is billed as usage billing bills owed.
func postUsageMonth(t *testing.T, w *world, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	nextMonth(w)
	closeMonths(t, fleet)
}

// unscheduledAgreement is alice's USD collection agreement.
func unscheduledAgreement(t *testing.T, a *authorityWorld) billing.Agreement {
	t.Helper()
	status, raw := a.request(t, http.MethodGet, "/v1/me/agreements", a.aliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var agreements billing.ListPage[billing.Agreement]
	require.NoError(t, json.Unmarshal(raw, &agreements))
	for _, agreement := range agreements.Items {
		if agreement.Subscription == nil && agreement.Currency == "USD" {
			return agreement
		}
	}
	require.Fail(t, "alice has no USD collection agreement")
	return billing.Agreement{}
}

func oneUsageOrder(t *testing.T, a *authorityWorld, customer billing.CustomerID) billing.Order {
	t.Helper()
	rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	require.NotNil(t, rows.Items[0].Usage)
	return rows.Items[0]
}

func TestUsageBillingContracts(t *testing.T) {
	t.Run("OR-J26-S01", func(t *testing.T) {
		for _, profile := range []string{"threshold", "monthly-date"} {
			t.Run(profile, func(t *testing.T) {
				a, p, customer := usageBillingWorld(t, true, false)
				units, native, cents := int64(3), int64(30_000_000), int64(3000)
				if profile == "monthly-date" {
					units, native, cents = 2, 20_000_000, 2000
				}
				oneUsage(t, a.alpha, usageInput(customer, "unbilled-work", units))
				a.clock.Advance(time.Second)
				fleet := startFleet(t, a.world)
				if profile == "monthly-date" {
					runUsageDue(t, fleet)
					orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
					require.NoError(t, err)
					require.Empty(t, orders.Items)
					p.money(t, 0, 0)
					a.clock.Advance(12 * time.Hour)
				}
				runUsageDue(t, fleet)
				require.Eventually(t, func() bool {
					rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
					return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.OrderComplete
				}, 15*time.Second, 20*time.Millisecond)
				order := oneUsageOrder(t, a, customer)
				require.Equal(t, native, order.Total)
				require.Equal(t, native, order.Usage.AmountPaid)
				require.Zero(t, order.Usage.AmountDue)
				runUsageDue(t, fleet)
				require.Equal(t, order.ID, oneUsageOrder(t, a, customer).ID)
				p.money(t, 1, cents)
				balance(t, a.alpha, customer, 0, 0, 0)
			})
		}
	})
	t.Run("OR-J26-S02/disabled-host-does-not-collect-automatic-wakeups", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, false, false)
		oneUsage(t, a.alpha, usageInput(customer, "debt-without-opt-in", 3))
		fleet := startFleet(t, a.world)
		nextMonth(a.world)
		closeMonths(t, fleet)
		runUsageDue(t, fleet)
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, orders.Items)
		p.money(t, 0, 0)
		funds, err := a.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
		require.NoError(t, err)
		require.EqualValues(t, 30_000_000, funds.OwedAmount)
		require.Zero(t, funds.BalanceAmount)
	})
	t.Run("OR-J26-S03", func(t *testing.T) {
		for _, disposition := range []string{"paid", "waived", "uncollectible"} {
			t.Run(disposition, func(t *testing.T) {
				a, p, customer := usageBillingWorld(t, true, true)
				oneUsage(t, a.alpha, usageInput(customer, "finished-work", 3))
				fleet := startFleet(t, a.world)
				postUsageMonth(t, a.world, fleet)
				runUsageDue(t, fleet)
				order := oneUsageOrder(t, a, customer)
				require.Equal(t, billing.OrderOpen, order.Status)
				var err error
				switch disposition {
				case "paid":
					_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 30_000_000, TransactionID: "bank-receipt-one"})
				case "waived":
					_, err = a.alpha.WaiveOrderBalance(t.Context(), order.ID)
				case "uncollectible":
					_, err = a.alpha.MarkOrderUncollectible(t.Context(), order.ID)
				}
				require.NoError(t, err)
				before := oneUsageOrder(t, a, customer)
				require.EqualValues(t, 30_000_000, before.Total)
				expectedStatus, expectedPaid, expectedDue, expectedOwed := billing.OrderComplete, int64(30_000_000), int64(0), int64(0)
				switch disposition {
				case "waived":
					expectedStatus, expectedPaid, expectedDue = billing.OrderVoid, 0, 0
					require.NotNil(t, before.VoidedAt)
				case "uncollectible":
					expectedStatus, expectedPaid, expectedDue, expectedOwed = billing.OrderUncollectible, 0, 30_000_000, 30_000_000
					require.NotNil(t, before.UncollectibleAt)
				}
				require.Equal(t, expectedStatus, before.Status)
				require.Equal(t, expectedPaid, before.Usage.AmountPaid)
				require.Equal(t, expectedDue, before.Usage.AmountDue)
				funds, err := a.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
				require.NoError(t, err)
				require.Equal(t, expectedOwed, funds.OwedAmount)
				require.Zero(t, funds.BalanceAmount)
				a.clock.Advance(35 * 24 * time.Hour)
				runUsageDue(t, fleet)
				after := oneUsageOrder(t, a, customer)
				require.Equal(t, before.ID, after.ID)
				require.Equal(t, expectedStatus, after.Status)
				require.Equal(t, expectedPaid, after.Usage.AmountPaid)
				require.Equal(t, expectedDue, after.Usage.AmountDue)
				funds, err = a.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
				require.NoError(t, err)
				require.Equal(t, expectedOwed, funds.OwedAmount)
				p.money(t, 0, 0)
			})
		}
	})
	t.Run("OR-J26-S04", func(t *testing.T) {
		for _, disposition := range []string{"paid", "waived"} {
			t.Run(disposition, func(t *testing.T) {
				a, p, customer := usageBillingWorld(t, true, true)
				oneUsage(t, a.alpha, usageInput(customer, "overdue-work", 3))
				fleet := startFleet(t, a.world)
				postUsageMonth(t, a.world, fleet)
				runUsageDue(t, fleet)
				a.clock.Advance(24 * time.Hour)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				order := oneUsageOrder(t, a, customer)
				require.True(t, order.Usage.Delinquent)
				var err error
				if disposition == "paid" {
					_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 30_000_000, TransactionID: "bank-cleared"})
				} else {
					_, err = a.alpha.WaiveOrderBalance(t.Context(), order.ID)
				}
				require.NoError(t, err)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				require.False(t, oneUsageOrder(t, a, customer).Usage.Delinquent)
				balance(t, a.alpha, customer, 0, 0, 0)
				events, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"delinquency.cleared"}, Limit: 100})
				require.NoError(t, err)
				require.Len(t, events.Events, 1)
				cleared := events.Events[0]
				require.Equal(t, "delinquency.cleared", cleared.Type)
				require.Equal(t, a.alpha.MerchantID(), cleared.MerchantID)
				require.Equal(t, customer.String(), cleared.Subject.ID)
				var transition struct {
					From     string `json:"from_state"`
					To       string `json:"to_state"`
					Currency string `json:"currency"`
				}
				err = json.Unmarshal(cleared.Data, &transition)
				require.NoError(t, err)
				require.Equal(t, "delinquent", transition.From)
				require.Equal(t, "current", transition.To)
				require.Equal(t, "USD", transition.Currency)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				repeated, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"delinquency.cleared"}, Limit: 100})
				require.NoError(t, err)
				require.Len(t, repeated.Events, 1, "replaying evaluation must not emit another clearance")
				p.money(t, 0, 0)
			})
		}
	})
	t.Run("OR-J26-S50", func(t *testing.T) {
		for _, disposition := range []string{"paid", "waived", "uncollectible"} {
			t.Run(disposition, func(t *testing.T) {
				a, p, customer := usageBillingWorld(t, true, true)
				oneUsage(t, a.alpha, usageInput(customer, "settled-at-once", 3))
				fleet := startFleet(t, a.world)
				postUsageMonth(t, a.world, fleet)
				runUsageDue(t, fleet)
				a.clock.Advance(24 * time.Hour)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				order := oneUsageOrder(t, a, customer)
				require.True(t, order.Usage.Delinquent)
				var err error
				switch disposition {
				case "paid":
					_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 30_000_000, TransactionID: "bank-at-once"})
				case "waived":
					_, err = a.alpha.WaiveOrderBalance(t.Context(), order.ID)
				case "uncollectible":
					_, err = a.alpha.MarkOrderUncollectible(t.Context(), order.ID)
				}
				require.NoError(t, err)
				cleared := func() int {
					events, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"delinquency.cleared"}, Limit: 100})
					require.NoError(t, err)
					return len(events.Events)
				}
				require.Equal(t, 1, cleared(), "the change itself clears the customer; no pass ran")
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				require.Equal(t, 1, cleared())
				p.money(t, 0, 0)
			})
		}
	})
	t.Run("OR-J26-S51/an-overdue-order-is-told-without-automatic-usage-billing", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, false, false)
		oneUsage(t, a.alpha, usageInput(customer, "billed-on-revocation", 3))
		// What the revocation bills is owed once the month's usage posts.
		fleet := startFleet(t, a.world)
		postUsageMonth(t, a.world, fleet)
		// Revoking the collection agreement bills what is owed so far.
		status, raw := a.request(t, http.MethodPost, "/v1/me/agreements/"+unscheduledAgreement(t, a).ID.String()+"/revoke", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		order := oneUsageOrder(t, a, customer)
		require.Equal(t, billing.OrderOpen, order.Status)
		a.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		notices, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"order_overdue"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, notices.Events, 1, "the overdue order is told although no usage pass runs")
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		notices, err = a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"order_overdue"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, notices.Events, 1, "told once")
	})
	t.Run("OR-J26-S40/usage-pass-bills-past-a-full-page-of-due-accounts", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, true, false)
		// Two hundred accounts due ahead of Alice's fill the pass's first page.
		mid := a.alpha.MerchantID().UUID()
		_, err := a.pool.Exec(t.Context(), `WITH c AS (INSERT INTO `+pgx.Identifier{a.schema, "customers"}.Sanitize()+` (id, merchant_id)
			SELECT ('00000000-0000-0000-0000-' || lpad(to_hex(g), 12, '0'))::uuid, $1 FROM generate_series(1, 200) g RETURNING id)
			INSERT INTO `+pgx.Identifier{a.schema, "money_settings"}.Sanitize()+` (merchant_id, customer_id, billing_mode, currency, usage_collection_due, usage_due_at)
			SELECT $1, id, 'arrears', 'USD', true, to_timestamp(0) FROM c`, mid)
		require.NoError(t, err)
		oneUsage(t, a.alpha, usageInput(customer, "past-the-first-page", 3))
		fleet := startFleet(t, a.world)
		postUsageMonth(t, a.world, fleet)
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.OrderComplete
		}, 30*time.Second, 50*time.Millisecond, "the pass continues past its first page")
		require.Eventually(t, func() bool {
			jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.usage_orders").States(rivertype.JobStateCompleted).First(100))
			require.NoError(t, err)
			for _, job := range jobs.Jobs {
				var args struct {
					Bill *struct{} `json:"bill"`
				}
				require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
				if args.Bill != nil {
					return true
				}
			}
			return false
		}, 20*time.Second, 50*time.Millisecond, "a continuation job billed the second page")
		p.money(t, 1, 3000)
	})
	t.Run("OR-J26-S52/an-account-billed-in-full-waits-for-its-next-spend", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, false)
		oneUsage(t, a.alpha, usageInput(customer, "billed-in-full", 2))
		fleet := startFleet(t, a.world)
		a.clock.Advance(12 * time.Hour)
		runUsageDue(t, fleet)
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.OrderComplete
		}, 15*time.Second, 20*time.Millisecond)
		nextBill := func() *time.Time {
			c, err := a.alpha.GetCustomer(t.Context(), customer)
			require.NoError(t, err)
			require.Len(t, c.Settings.UsageBilling, 1)
			return c.Settings.UsageBilling[0].NextBillAt
		}
		require.Nil(t, nextBill(), "nothing is pending, so no bill date wakes the account")
		oneUsage(t, a.alpha, usageInput(customer, "spent-again", 1))
		next := nextBill()
		require.NotNil(t, next)
		require.Equal(t, time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), next.UTC())
	})
	t.Run("OR-J26-S53/usage-pass-queues-the-charge-without-waiting-on-the-gateway", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, true, false)
		hold := p.core.Hold(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/transact.php") }, nmimock.HoldRequest)
		defer hold.Release()
		oneUsage(t, a.alpha, usageInput(customer, "queued-collection", 3))
		fleet := startFleet(t, a.world)
		postUsageMonth(t, a.world, fleet)
		runUsageDue(t, fleet)
		select {
		case <-hold.Arrived():
		case <-time.After(20 * time.Second):
			t.Fatal("the queued collection never charged")
		}
		running := func(kind string) int {
			jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds(kind).States(rivertype.JobStateRunning))
			require.NoError(t, err)
			return len(jobs.Jobs)
		}
		require.Eventually(t, func() bool { return running("openrails.usage_orders") == 0 }, 10*time.Second, 50*time.Millisecond,
			"no usage pass waits on the gateway")
		require.Equal(t, 1, running("openrails.provider_operation"), "the collection's own operation job charges")
		hold.Release()
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.OrderComplete
		}, 15*time.Second, 20*time.Millisecond)
		p.money(t, 1, 3000)
	})
	t.Run("OR-J26-S54/a-running-charge-is-billed-through-the-bill-date-before-its-daily-posting", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, true)
		oneUsage(t, a.alpha, usageInput(customer, "before-the-bill-date", 1))
		_, err := a.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "through-the-bill-date", CustomerID: customer, Currency: "USD", RatePerHour: 360_000})
		require.NoError(t, err)
		// No balance watch runs, so nothing posts the charge at 00:00 UTC.
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		a.clock.Advance(12*time.Hour + 10*time.Second)
		runUsageDue(t, fleet)
		order := oneUsageOrder(t, a, customer)
		// 10 USD of usage and 12 h 10 s at 0.36 USD an hour, to the pass.
		require.EqualValues(t, 14_321_000, order.Total)
		require.Equal(t, time.Date(2026, 9, 25, 0, 0, 10, 0, time.UTC), order.Usage.PeriodEndsAt.UTC().Truncate(time.Second))
	})
	t.Run("OR-J26-S56/usage-reported-after-its-days-order-is-owed-after-that-order", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, true)
		oneUsage(t, a.alpha, usageInput(customer, "on-time", 1))
		fleet := quietFleet(t, a.world)
		a.clock.Advance(13 * time.Hour)
		runUsageDue(t, fleet)
		first := oneUsageOrder(t, a, customer)
		require.EqualValues(t, 10_000_000, first.Total)
		// Usage of the billed day reported an hour after its order, enough to
		// reach the threshold: it is billed at once, in a period no order
		// billed.
		a.clock.Advance(time.Hour)
		late := usageInput(customer, "late", 3)
		late.OccurredAt = time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
		oneUsage(t, a.alpha, late)
		runUsageDue(t, fleet)
		rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, rows.Items, 2)
		second := rows.Items[0]
		if second.ID == first.ID {
			second = rows.Items[1]
		}
		require.NotNil(t, second.Usage)
		require.EqualValues(t, 30_000_000, second.Total)
		require.False(t, second.Usage.PeriodStartsAt.Before(first.Usage.PeriodEndsAt), "late usage is owed after the order that billed its day: %s < %s",
			second.Usage.PeriodStartsAt, first.Usage.PeriodEndsAt)
	})
	t.Run("OR-J26-S57/a-period-that-ends-within-a-day-bills-that-days-usage-in-it", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, true)
		// The customer's periods turn at 14:00 on the 25th.
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{UsageBilling: []billing.UsageBillingAccountUpdate{{Currency: "USD",
			BillingAnchor: catalog.Value(time.Date(2026, 1, 25, 14, 0, 0, 0, time.UTC))}}})
		require.NoError(t, err)
		fleet := quietFleet(t, a.world)
		a.clock.Advance(22 * time.Hour)
		oneUsage(t, a.alpha, usageInput(customer, "before-the-turn", 1))
		// The bill date's pass, after the turn, posts the day so far: what
		// occurred before 14:00 is owed in the period that ended then.
		a.clock.Advance(5 * time.Hour)
		runUsageDue(t, fleet)
		order := oneUsageOrder(t, a, customer)
		require.EqualValues(t, 10_000_000, order.Total)
		turn := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
		require.True(t, order.Usage.PeriodStartsAt.Before(turn), "owed at %s, after its period ended", order.Usage.PeriodStartsAt)
	})
	t.Run("OR-J26-S58/revoking-the-agreement-bills-todays-usage", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, false, false)
		oneUsage(t, a.alpha, usageInput(customer, "today", 3))
		// No day ended and no pass ran: the revocation posts today's usage on
		// its own clock and bills it.
		a.clock.Advance(time.Second)
		status, raw := a.request(t, http.MethodPost, "/v1/me/agreements/"+unscheduledAgreement(t, a).ID.String()+"/revoke", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var revoked billing.AgreementRevocation
		require.NoError(t, json.Unmarshal(raw, &revoked))
		require.NotNil(t, revoked.UsageOrder, "the final bill")
		require.EqualValues(t, 30_000_000, revoked.UsageOrder.Total)
		require.Equal(t, revoked.UsageOrder.ID, oneUsageOrder(t, a, customer).ID)
	})
	t.Run("OR-J26-S59/a-declined-usage-collection-retries-on-the-merchants-dunning-policy", func(t *testing.T) {
		// Every tier's first retry differs from the built-in schedule's (1 and 2 days).
		a, p, customer := usageBillingWorld(t, true, false, dunningPolicy(billing.DunningPolicy{Tiers: []billing.DunningTier{
			{MaxCycleHours: 96}, {MaxCycleHours: 672, RetryAfterHours: []int{12, 36}}, {RetryAfterHours: []int{36, 84, 132, 204}},
		}}))
		p.core.SetDecline("4242", "202")
		oneUsage(t, a.alpha, usageInput(customer, "declined-collection", 3))
		a.clock.Advance(time.Second)
		fleet := startFleet(t, a.world)
		declinedAt := a.clock.Now()
		runUsageDue(t, fleet)
		var order billing.Order
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
			if err != nil || len(rows.Items) != 1 || rows.Items[0].Usage == nil {
				return false
			}
			order = rows.Items[0]
			return order.Payment.LastPaymentError != nil
		}, 15*time.Second, 20*time.Millisecond, "the collection is declined")
		require.Equal(t, billing.OrderOpen, order.Status, "a retryable decline keeps the order open")
		retry := 12 * time.Hour
		if cycle := order.Usage.PeriodEndsAt.Sub(order.Usage.PeriodStartsAt); cycle >= 672*time.Hour {
			retry = 36 * time.Hour
		}
		require.NotNil(t, order.Usage.NextCollectionAttemptAt)
		require.Equal(t, declinedAt.Add(retry), *order.Usage.NextCollectionAttemptAt, "the merchant's schedule sets the retry")
	})
	t.Run("OR-J26-S60/a-policys-delinquency-floor-holds-only-debt-in-its-currency", func(t *testing.T) {
		// The policy's floor is a million euros; the debt is in dollars.
		a, _, customer := usageBillingWorld(t, true, true, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "euro-floor", Kind: "outstanding_cap", OutstandingCapAmount: 1_000_000_000_000,
				DelinquencyGraceDays: ptr(0), DelinquencyAmountFloor: ptr(int64(1_000_000_000_000)), PolicyCurrency: "EUR"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{BillingPolicy: catalog.Value("euro-floor")})
		require.NoError(t, err)
		oneUsage(t, a.alpha, usageInput(customer, "overdue-in-dollars", 3))
		fleet := startFleet(t, a.world)
		postUsageMonth(t, a.world, fleet)
		runUsageDue(t, fleet)
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.True(t, oneUsageOrder(t, a, customer).Usage.Delinquent, "dollar debt is held to the dollar floor, not the policy's euro one")
	})
	t.Run("OR-J26-S61/a-policys-cap-in-another-currency-binds-no-dollar-money", func(t *testing.T) {
		// A yen policy caps debt at ¥1,000 and collects at ¥1,000; the dollar
		// account has its own $5,000 line and collects at $30.
		a, p, customer := usageBillingWorld(t, true, false, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.UsageBilling.Currencies = []billing.UsageBillingCurrency{{Currency: "USD", CreditLimit: 5_000_000_000, MinimumInvoiceAmount: 1_000_000,
				CollectionThreshold: ptr(int64(30_000_000)), DelinquencyFloor: ptr(int64(0))}}
			m.BillingPolicies = []billing.BillingPolicy{{Name: "yen", Kind: "outstanding_cap", PolicyCurrency: "JPY", OutstandingCapAmount: 10_000_000, CollectionThresholdAmount: ptr(int64(10_000_000))}}
			m.BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "yen"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		// $30 of dollar usage reaches the dollar threshold and is collected at
		// once: the yen threshold, at the fixture's rate of 1 $1,000, withholds
		// nothing.
		oneUsage(t, a.alpha, usageInput(customer, "dollar-work", 3))
		a.clock.Advance(time.Second)
		fleet := startFleet(t, a.world)
		runUsageDue(t, fleet)
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.OrderComplete
		}, 15*time.Second, 20*time.Millisecond, "the dollar threshold collects")
		require.EqualValues(t, 30_000_000, oneUsageOrder(t, a, customer).Total)
		p.money(t, 1, 3000)
		// A $1,500 request is within the dollar line, which the yen cap does
		// not bind.
		results, err := a.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(a.world, customer, "past-the-yen-cap", 1_500_000_000)})
		require.NoError(t, err)
		require.True(t, results[0].Allowed(), "%+v", results[0].Admission)
	})
	t.Run("OR-J26-S30/admission-applies-the-customers-own-delinquency-policy", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, true, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.ArrearsGraceDays = ptr(30)
			m.BillingPolicies = []billing.BillingPolicy{{Name: "strict", Kind: "outstanding_cap", OutstandingCapAmount: 1_000_000_000_000, DelinquencyGraceDays: ptr(0), DelinquencyAmountFloor: ptr(int64(0)), PolicyCurrency: "USD"}}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{BillingPolicy: catalog.Value("strict")})
		require.NoError(t, err)
		oneUsage(t, a.alpha, usageInput(customer, "overdue-under-strict-policy", 3))
		fleet := startFleet(t, a.world)
		postUsageMonth(t, a.world, fleet)
		runUsageDue(t, fleet)
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.True(t, oneUsageOrder(t, a, customer).Usage.Delinquent, "the customer's policy has no grace")
		results, err := a.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(a.world, customer, "after-strict-grace", 500)})
		require.NoError(t, err)
		require.False(t, results[0].Allowed())
		require.NotNil(t, results[0].Admission)
		require.NotNil(t, results[0].Admission.DenyCode)
		require.Equal(t, billing.DenyDelinquentUnpaidOrder, *results[0].Admission.DenyCode)
	})
}
