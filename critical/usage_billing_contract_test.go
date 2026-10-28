//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
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
	changes := append([]func(*world){func(w *world) {
		w.clock.Advance(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC).Sub(w.clock.Now()))
		w.cfg.UsageBillingEnabled = enabled
		m := w.cfg.Merchants["alpha"]
		m.UsageBilling = &billing.UsageBillingSettings{BillingDay: 25, Currencies: []billing.UsageBillingCurrency{{Currency: "USD", CreditLimit: 30_000_000, MinimumInvoiceAmount: 1_000_000}}}
		m.ArrearsGraceDays, m.ArrearsDelinquencyFloor = ptr(0), ptr(int64(0))
		w.cfg.Merchants["alpha"] = m
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
	usageMeter(t, a.alpha, 10_000_000)
	return a, p, customer
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
		a.clock.Advance(48 * time.Hour)
		fleet := startFleet(t, a.world)
		runUsageDue(t, fleet)
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, orders.Items)
		p.money(t, 0, 0)
		funds, err := a.alpha.GetBalance(t.Context(), customer, "USD")
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
				funds, err := a.alpha.GetBalance(t.Context(), customer, "USD")
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
				funds, err = a.alpha.GetBalance(t.Context(), customer, "USD")
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
				runUsageDue(t, fleet)
				a.clock.Advance(24 * time.Hour)
				runRegistered(t, fleet, "openrails.delinquency", 1)
				order := oneUsageOrder(t, a, customer)
				require.True(t, order.Usage.Delinquent)
				var err error
				if disposition == "paid" {
					_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 30_000_000, TransactionID: "bank-cleared"})
				} else {
					_, err = a.alpha.WaiveOrderBalance(t.Context(), order.ID)
				}
				require.NoError(t, err)
				runRegistered(t, fleet, "openrails.delinquency", 1)
				require.False(t, oneUsageOrder(t, a, customer).Usage.Delinquent)
				balance(t, a.alpha, customer, 0, 0, 0)
				events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"delinquency.cleared"}, Limit: 100})
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
				runRegistered(t, fleet, "openrails.delinquency", 1)
				repeated, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"delinquency.cleared"}, Limit: 100})
				require.NoError(t, err)
				require.Len(t, repeated.Events, 1, "replaying evaluation must not emit another clearance")
				p.money(t, 0, 0)
			})
		}
	})
	t.Run("OR-J26-S40/usage-pass-bills-past-a-full-page-of-due-accounts", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, true, false)
		// Two hundred accounts due ahead of Alice's fill the pass's first page.
		mid := a.alpha.MerchantID().UUID()
		_, err := a.pool.Exec(t.Context(), `WITH c AS (INSERT INTO `+pgx.Identifier{a.schema, "customers"}.Sanitize()+` (id, merchant_id)
			SELECT ('00000000-0000-0000-0000-' || lpad(to_hex(g), 12, '0'))::uuid, $1 FROM generate_series(1, 200) g RETURNING id)
			INSERT INTO `+pgx.Identifier{a.schema, "money_settings"}.Sanitize()+` (merchant_id, customer_id, billing_mode, currency, usage_collection_due)
			SELECT $1, id, 'arrears', 'USD', true FROM c`, mid)
		require.NoError(t, err)
		oneUsage(t, a.alpha, usageInput(customer, "past-the-first-page", 3))
		fleet := startFleet(t, a.world)
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.OrderComplete
		}, 30*time.Second, 50*time.Millisecond, "the merchant's pass continues past its first page")
		require.Eventually(t, func() bool {
			jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.usage_orders").States(rivertype.JobStateCompleted).First(100))
			require.NoError(t, err)
			for _, job := range jobs.Jobs {
				var args struct {
					After *struct{} `json:"after"`
				}
				require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
				if args.After != nil {
					return true
				}
			}
			return false
		}, 20*time.Second, 50*time.Millisecond, "a continuation job billed the second page")
		p.money(t, 1, 3000)
	})
	t.Run("OR-J26-S30/admission-applies-the-customers-own-delinquency-policy", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, true, func(w *world) {
			m := w.cfg.Merchants["alpha"]
			m.ArrearsGraceDays = ptr(30)
			m.BillingPolicies = []billing.BillingPolicy{{Name: "strict", Kind: "outstanding_cap", OutstandingCapAmount: 1_000_000_000_000, DelinquencyGraceDays: ptr(0), DelinquencyAmountFloor: ptr(int64(0))}}
			w.cfg.Merchants["alpha"] = m
		})
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{BillingPolicy: catalog.Value("strict")})
		require.NoError(t, err)
		oneUsage(t, a.alpha, usageInput(customer, "overdue-under-strict-policy", 3))
		fleet := startFleet(t, a.world)
		runUsageDue(t, fleet)
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.delinquency", 1)
		require.True(t, oneUsageOrder(t, a, customer).Usage.Delinquent, "the customer's policy has no grace")
		results, err := a.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(a.world, customer, "after-strict-grace", 500)})
		require.NoError(t, err)
		require.False(t, results[0].Allowed())
		require.NotNil(t, results[0].Admission)
		require.NotNil(t, results[0].Admission.DenyCode)
		require.Equal(t, "delinquent_unpaid_order", *results[0].Admission.DenyCode)
	})
}
