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
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
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

func usageBillingWorld(t *testing.T, manual bool, configure ...func(*world)) (*authorityWorld, *nmiJournal, billing.CustomerID) {
	t.Helper()
	a, p, customer := usageBillingAccount(t, manual, configure...)
	usageMeter(t, a.alpha, 10_000_000)
	return a, p, customer
}

// usageBillingAccount is alice with a saved card and a $30 USD credit line,
// billed on the 25th: the merchant's usage_billing terms bill it.
func usageBillingAccount(t *testing.T, manual bool, configure ...func(*world)) (*authorityWorld, *nmiJournal, billing.CustomerID) {
	t.Helper()
	changes := append([]func(*world){func(w *world) {
		w.clock.Advance(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC).Sub(w.clock.Now()))
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
	status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: p.core.Tokenize(nmimock.Card{Last4: "4242", Exp: "1235"}), BillingDetails: billedTo("US", "10001")})
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

// owedAccruedAt is when the earliest owed item an invoice bills accrued.
func owedAccruedAt(t *testing.T, w *world, invoice billing.InvoiceID) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT min(accrued_at) FROM "+pgx.Identifier{w.schema, "owed_items"}.Sanitize()+" WHERE invoice_id = $1",
		invoice.UUID()).Scan(&at))
	return at.UTC()
}

// postUsageMonth ends the month the usage occurred: it posts, and what it
// leaves owed is billed as usage billing bills owed.
func postUsageMonth(t *testing.T, w *world, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	nextMonth(w)
	postDays(t, fleet)
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

func oneUsageInvoice(t *testing.T, a *authorityWorld, customer billing.CustomerID) billing.Invoice {
	t.Helper()
	rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	require.NotNil(t, rows.Items[0].Usage)
	return rows.Items[0]
}

func TestUsageBillingContracts(t *testing.T) {
	t.Run("OR-J26-S01", func(t *testing.T) {
		for _, profile := range []string{"threshold", "monthly-date"} {
			t.Run(profile, func(t *testing.T) {
				a, p, customer := usageBillingWorld(t, false)
				units, native, cents := int64(3), int64(30_000_000), int64(3000)
				if profile == "monthly-date" {
					units, native, cents = 2, 20_000_000, 2000
				}
				oneUsage(t, a.alpha, usageInput(customer, "unbilled-work", units))
				a.clock.Advance(time.Second)
				fleet := startFleet(t, a.world)
				if profile == "monthly-date" {
					runUsageDue(t, fleet)
					invoices, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
					require.NoError(t, err)
					require.Empty(t, invoices.Items)
					p.money(t, 0, 0)
					a.clock.Advance(12 * time.Hour)
				}
				runUsageDue(t, fleet)
				require.Eventually(t, func() bool {
					rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
					return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.InvoiceComplete
				}, 15*time.Second, 20*time.Millisecond)
				invoice := oneUsageInvoice(t, a, customer)
				require.Equal(t, native, invoice.Total)
				require.Equal(t, native, invoice.Usage.AmountPaid)
				require.Zero(t, invoice.Usage.AmountDue)
				runUsageDue(t, fleet)
				require.Equal(t, invoice.ID, oneUsageInvoice(t, a, customer).ID)
				p.money(t, 1, cents)
				balance(t, a.alpha, customer, 0, 0, 0)
			})
		}
		t.Run("lowered-threshold", func(t *testing.T) {
			// Lowering the collection threshold bills debt already above it,
			// not at the bill date.
			a, _ := newPSPWorld(t, func(w *world) {
				w.clock.Advance(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC).Sub(w.clock.Now()))
			})
			state, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{DisplayName: ptr("Alpha"),
				MerchantSettings: &billing.MerchantSettings{UsageBilling: &billing.UsageBillingSettings{BillingDay: 25,
					Currencies: []billing.UsageBillingCurrency{{Currency: "USD", CreditLimit: 30_000_000, CollectionThreshold: ptr(int64(25_000_000)), MinimumInvoiceAmount: 1_000_000}}}}})
			require.NoError(t, err)
			customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
			_, err = a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{InvoiceProfile: catalog.Value(billing.InvoiceProfile{CollectionMethod: billing.CollectSendInvoice})})
			require.NoError(t, err)
			usageMeter(t, a.alpha, 10_000_000)
			oneUsage(t, a.alpha, usageInput(customer, "below-old-threshold", 2))
			nextDay(a.world)
			fleet := quietFleet(t, a.world)
			postDays(t, fleet)
			runUsageDue(t, fleet)
			require.EqualValues(t, 20_000_000, owed(t, a.alpha, customer))
			rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
			require.NoError(t, err)
			require.Empty(t, rows.Items, "$20 owed waits under a $25 threshold")
			state.UsageBilling.Currencies[0].CollectionThreshold = ptr(int64(10_000_000))
			_, err = a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &state.MerchantSettings})
			require.NoError(t, err)
			// What a change to the merchant's settings runs, on the settings
			// the write committed.
			require.NoError(t, embedded.Graph(a.engine).Runtime.MoneyService.MarkUsageAccountsDue(merchant.WithID(t.Context(), a.alpha.MerchantID())))
			runUsageDue(t, fleet)
			invoice := oneUsageInvoice(t, a, customer)
			require.Equal(t, billing.InvoiceOpen, invoice.Status)
			require.EqualValues(t, 20_000_000, invoice.Total)
		})
	})
	t.Run("OR-J26-S02/a-merchant-without-usage-terms-bills-no-usage", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, false, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.UsageBilling = nil
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		oneUsage(t, a.alpha, usageInput(customer, "debt-without-opt-in", 3))
		fleet := startFleet(t, a.world)
		nextMonth(a.world)
		postDays(t, fleet)
		runUsageDue(t, fleet)
		invoices, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, invoices.Items)
		p.money(t, 0, 0)
		funds, err := a.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
		require.NoError(t, err)
		require.EqualValues(t, 30_000_000, funds.OwedAmount)
		require.Zero(t, funds.BalanceAmount)
	})
	t.Run("OR-J26-S03", func(t *testing.T) {
		for _, disposition := range []string{"paid", "waived", "uncollectible"} {
			t.Run(disposition, func(t *testing.T) {
				a, p, customer := usageBillingWorld(t, true)
				oneUsage(t, a.alpha, usageInput(customer, "finished-work", 3))
				fleet := startFleet(t, a.world)
				postUsageMonth(t, a.world, fleet)
				runUsageDue(t, fleet)
				invoice := oneUsageInvoice(t, a, customer)
				require.Equal(t, billing.InvoiceOpen, invoice.Status)
				var err error
				switch disposition {
				case "paid":
					_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{InvoiceID: invoice.ID, Amount: 30_000_000, TransactionID: "bank-receipt-one"})
				case "waived":
					_, err = a.alpha.WaiveInvoiceBalance(t.Context(), invoice.ID)
				case "uncollectible":
					_, err = a.alpha.MarkInvoiceUncollectible(t.Context(), invoice.ID)
				}
				require.NoError(t, err)
				before := oneUsageInvoice(t, a, customer)
				require.EqualValues(t, 30_000_000, before.Total)
				expectedStatus, expectedPaid, expectedDue, expectedOwed := billing.InvoiceComplete, int64(30_000_000), int64(0), int64(0)
				switch disposition {
				case "waived":
					expectedStatus, expectedPaid, expectedDue = billing.InvoiceVoid, 0, 0
					require.NotNil(t, before.VoidedAt)
				case "uncollectible":
					expectedStatus, expectedPaid, expectedDue, expectedOwed = billing.InvoiceUncollectible, 0, 30_000_000, 30_000_000
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
				after := oneUsageInvoice(t, a, customer)
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
				a, p, customer := usageBillingWorld(t, true)
				oneUsage(t, a.alpha, usageInput(customer, "overdue-work", 3))
				fleet := startFleet(t, a.world)
				postUsageMonth(t, a.world, fleet)
				runUsageDue(t, fleet)
				a.clock.Advance(24 * time.Hour)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				invoice := oneUsageInvoice(t, a, customer)
				require.True(t, invoice.Usage.Delinquent)
				var err error
				if disposition == "paid" {
					_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{InvoiceID: invoice.ID, Amount: 30_000_000, TransactionID: "bank-cleared"})
				} else {
					_, err = a.alpha.WaiveInvoiceBalance(t.Context(), invoice.ID)
				}
				require.NoError(t, err)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				require.False(t, oneUsageInvoice(t, a, customer).Usage.Delinquent)
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
				a, p, customer := usageBillingWorld(t, true)
				oneUsage(t, a.alpha, usageInput(customer, "settled-at-once", 3))
				fleet := startFleet(t, a.world)
				postUsageMonth(t, a.world, fleet)
				runUsageDue(t, fleet)
				a.clock.Advance(24 * time.Hour)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				invoice := oneUsageInvoice(t, a, customer)
				require.True(t, invoice.Usage.Delinquent)
				var err error
				switch disposition {
				case "paid":
					_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{InvoiceID: invoice.ID, Amount: 30_000_000, TransactionID: "bank-at-once"})
				case "waived":
					_, err = a.alpha.WaiveInvoiceBalance(t.Context(), invoice.ID)
				case "uncollectible":
					_, err = a.alpha.MarkInvoiceUncollectible(t.Context(), invoice.ID)
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
	t.Run("OR-J26-S51/an-overdue-invoice-is-told-once-by-the-billing-pass", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, false)
		// The card declines, so the invoice stays unpaid past its due date.
		p.core.SetDecline("4242", "202")
		oneUsage(t, a.alpha, usageInput(customer, "billed-on-revocation", 3))
		// What the revocation bills is owed once the month's usage posts.
		fleet := startFleet(t, a.world)
		postUsageMonth(t, a.world, fleet)
		// Revoking the collection agreement bills what is owed so far.
		status, raw := a.request(t, http.MethodPost, "/v1/me/agreements/"+unscheduledAgreement(t, a).ID.String()+"/revoke", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Eventually(t, func() bool { return oneUsageInvoice(t, a, customer).Status == billing.InvoiceOpen }, 15*time.Second, 50*time.Millisecond,
			"the declined collection leaves the invoice open")
		a.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		notices, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"invoice_overdue"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, notices.Events, 1, "the billing pass tells the overdue invoice")
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		notices, err = a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"invoice_overdue"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, notices.Events, 1, "told once")
	})
	t.Run("OR-J26-S40/usage-pass-bills-past-a-full-page-of-due-accounts", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, false)
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
			rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.InvoiceComplete
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
		a, _, customer := usageBillingWorld(t, false)
		oneUsage(t, a.alpha, usageInput(customer, "billed-in-full", 2))
		fleet := startFleet(t, a.world)
		a.clock.Advance(12 * time.Hour)
		runUsageDue(t, fleet)
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.InvoiceComplete
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
		a, p, customer := usageBillingWorld(t, false)
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
			rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.InvoiceComplete
		}, 15*time.Second, 20*time.Millisecond)
		p.money(t, 1, 3000)
	})
	t.Run("OR-J26-S54/a-running-charge-is-billed-through-the-bill-date-before-its-daily-posting", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true)
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
		invoice := oneUsageInvoice(t, a, customer)
		// 10 USD of usage and 12 h 10 s at 0.36 USD an hour, to the pass.
		require.EqualValues(t, 14_321_000, invoice.Total)
		require.Equal(t, time.Date(2026, 9, 25, 0, 0, 10, 0, time.UTC), invoice.Usage.PeriodEndsAt.UTC().Truncate(time.Second))
	})
	t.Run("OR-J26-S64/running-charges-bill-on-the-merchants-usage-terms-without-clickhouse", func(t *testing.T) {
		// No host setting turns usage billing on, and the deployment has no
		// event store: the merchant's usage_billing terms bill its running
		// charges from PostgreSQL alone.
		a, _, customer := usageBillingAccount(t, true, func(w *world) { w.deps.ClickHouse = nil })
		_, err := a.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "without-the-store", CustomerID: customer, Currency: "USD", RatePerHour: 360_000})
		require.NoError(t, err)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		// Starting the charge gave the account its bill date, the 25th.
		a.clock.Advance(12*time.Hour + 10*time.Second)
		runUsageDue(t, fleet)
		invoice := oneUsageInvoice(t, a, customer)
		// 12 h 10 s at 0.36 USD an hour, to the pass.
		require.EqualValues(t, 4_321_000, invoice.Total)
		require.Equal(t, time.Date(2026, 9, 25, 0, 0, 10, 0, time.UTC), invoice.Usage.PeriodEndsAt.UTC().Truncate(time.Second))
	})
	t.Run("OR-J26-S56/usage-reported-after-its-days-invoice-is-owed-after-that-invoice", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true)
		oneUsage(t, a.alpha, usageInput(customer, "on-time", 1))
		fleet := quietFleet(t, a.world)
		a.clock.Advance(13 * time.Hour)
		runUsageDue(t, fleet)
		first := oneUsageInvoice(t, a, customer)
		require.EqualValues(t, 10_000_000, first.Total)
		// Usage of the billed day reported an hour after its invoice, enough to
		// reach the threshold: it is billed at once, in a period no invoice
		// billed.
		a.clock.Advance(time.Hour)
		late := usageInput(customer, "late", 3)
		late.OccurredAt = time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
		oneUsage(t, a.alpha, late)
		runUsageDue(t, fleet)
		rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, rows.Items, 2)
		second := rows.Items[0]
		if second.ID == first.ID {
			second = rows.Items[1]
		}
		require.NotNil(t, second.Usage)
		require.EqualValues(t, 30_000_000, second.Total)
		require.False(t, owedAccruedAt(t, a.world, second.ID).Before(first.Usage.PeriodEndsAt), "late usage is owed after the invoice that billed its day")
		require.True(t, second.Usage.PeriodEndsAt.After(first.Usage.PeriodEndsAt))
		require.False(t, second.Usage.PeriodStartsAt.After(late.OccurredAt), "its invoice's period covers it: %s", second.Usage.PeriodStartsAt)
	})
	t.Run("OR-J26-S57/a-period-that-ends-within-a-day-bills-that-days-usage-in-it", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true)
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
		invoice := oneUsageInvoice(t, a, customer)
		require.EqualValues(t, 10_000_000, invoice.Total)
		turn := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
		require.True(t, invoice.Usage.PeriodStartsAt.Before(turn), "owed at %s, after its period ended", invoice.Usage.PeriodStartsAt)
	})
	t.Run("OR-J26-S63/a-usage-invoices-period-covers-the-usage-it-bills", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true)
		occurred := a.clock.Now().UTC()
		oneUsage(t, a.alpha, usageInput(customer, "in-its-period", 3))
		fleet := startFleet(t, a.world)
		// The close owes it at its pricing period's last instant; the invoice
		// that bills it states the period its charge came from.
		postUsageMonth(t, a.world, fleet)
		runUsageDue(t, fleet)
		invoice := oneUsageInvoice(t, a, customer)
		require.EqualValues(t, 30_000_000, invoice.Total)
		require.NotNil(t, invoice.Usage)
		require.False(t, invoice.Usage.PeriodStartsAt.After(occurred), "the period %s..%s covers the usage at %s",
			invoice.Usage.PeriodStartsAt, invoice.Usage.PeriodEndsAt, occurred)
		require.True(t, occurred.Before(invoice.Usage.PeriodEndsAt))
		require.False(t, invoice.Usage.PeriodEndsAt.Before(a.clock.Now()), "and runs to its bill")
	})
	t.Run("OR-J26-S58/revoking-the-agreement-bills-todays-usage", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, false)
		oneUsage(t, a.alpha, usageInput(customer, "today", 3))
		// No day ended and no pass ran: the revocation posts today's usage on
		// its own clock and bills it.
		a.clock.Advance(time.Second)
		status, raw := a.request(t, http.MethodPost, "/v1/me/agreements/"+unscheduledAgreement(t, a).ID.String()+"/revoke", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var revoked billing.AgreementRevocation
		require.NoError(t, json.Unmarshal(raw, &revoked))
		require.NotNil(t, revoked.UsageInvoice, "the final bill")
		require.EqualValues(t, 30_000_000, revoked.UsageInvoice.Total)
		require.Equal(t, revoked.UsageInvoice.ID, oneUsageInvoice(t, a, customer).ID)
	})
	t.Run("OR-J26-S59/a-declined-usage-collection-retries-on-the-merchants-dunning-policy", func(t *testing.T) {
		// Every tier's first retry differs from the built-in schedule's (1 and 2 days).
		a, p, customer := usageBillingWorld(t, false, dunningPolicy(billing.DunningPolicy{Tiers: []billing.DunningTier{
			{MaxCycleHours: 96}, {MaxCycleHours: 672, RetryAfterHours: []int{12, 36}}, {RetryAfterHours: []int{36, 84, 132, 204}},
		}}))
		p.core.SetDecline("4242", "202")
		oneUsage(t, a.alpha, usageInput(customer, "declined-collection", 3))
		a.clock.Advance(time.Second)
		fleet := startFleet(t, a.world)
		declinedAt := a.clock.Now()
		runUsageDue(t, fleet)
		var invoice billing.Invoice
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
			if err != nil || len(rows.Items) != 1 || rows.Items[0].Usage == nil {
				return false
			}
			invoice = rows.Items[0]
			return invoice.Payment.LastPaymentError != nil
		}, 15*time.Second, 20*time.Millisecond, "the collection is declined")
		require.Equal(t, billing.InvoiceOpen, invoice.Status, "a retryable decline keeps the invoice open")
		retry := 12 * time.Hour
		if cycle := invoice.Usage.PeriodEndsAt.Sub(invoice.Usage.PeriodStartsAt); cycle >= 672*time.Hour {
			retry = 36 * time.Hour
		}
		require.NotNil(t, invoice.Usage.NextCollectionAttemptAt)
		require.Equal(t, declinedAt.Add(retry), *invoice.Usage.NextCollectionAttemptAt, "the merchant's schedule sets the retry")
	})
	t.Run("OR-J26-S60/a-policys-delinquency-floor-holds-only-debt-in-its-currency", func(t *testing.T) {
		// The policy's floor is a million euros; the debt is in dollars.
		a, _, customer := usageBillingWorld(t, true, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.BillingPolicies = []billing.BillingPolicy{{Name: "euro-floor",
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
		require.True(t, oneUsageInvoice(t, a, customer).Usage.Delinquent, "dollar debt is held to the dollar floor, not the policy's euro one")
	})
	t.Run("OR-J26-S61/a-policys-cap-in-another-currency-binds-no-dollar-money", func(t *testing.T) {
		// A yen policy caps debt at ¥1,000 and collects at ¥1,000; the dollar
		// account has its own $5,000 line and collects at $30.
		a, p, customer := usageBillingWorld(t, false, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.UsageBilling.Currencies = []billing.UsageBillingCurrency{{Currency: "USD", CreditLimit: 5_000_000_000, MinimumInvoiceAmount: 1_000_000,
				CollectionThreshold: ptr(int64(30_000_000)), DelinquencyFloor: ptr(int64(0))}}
			m.BillingPolicies = []billing.BillingPolicy{{Name: "yen", PolicyCurrency: "JPY", OutstandingCapAmount: 10_000_000, CollectionThresholdAmount: ptr(int64(10_000_000))}}
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
			rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.InvoiceComplete
		}, 15*time.Second, 20*time.Millisecond, "the dollar threshold collects")
		require.EqualValues(t, 30_000_000, oneUsageInvoice(t, a, customer).Total)
		p.money(t, 1, 3000)
		// A $1,500 request is within the dollar line, which the yen cap does
		// not bind.
		out := syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
		set := out.LimitSets[0]
		require.Nil(t, set.Refusal)
		require.GreaterOrEqual(t, lineOf(t, viewOf(t, out, customer), catalog.BalanceLimit).Remaining+set.CreditLine, int64(1_500_000_000))
	})
	t.Run("OR-J26-S30/a-view-applies-the-customers-own-delinquency-policy", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.ArrearsGraceDays = ptr(30)
			m.BillingPolicies = []billing.BillingPolicy{{Name: "strict", DelinquencyGraceDays: ptr(0), DelinquencyAmountFloor: ptr(int64(0)), PolicyCurrency: "USD"}}
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
		require.True(t, oneUsageInvoice(t, a, customer).Usage.Delinquent, "the customer's policy has no grace")
		out := syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
		require.NotNil(t, out.LimitSets[0].Refusal)
		require.Equal(t, billing.DenyDelinquentUnpaidInvoice, out.LimitSets[0].Refusal.DenyCode)
	})
}
