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

type usageDueJob struct {
	Automatic bool `json:"automatic"`
}

func (usageDueJob) Kind() string { return "openrails.invoice" }

func runUsageDue(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	for range 2 {
		job, err := fleet.Insert(t.Context(), usageDueJob{Automatic: true}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			row, err := fleet.JobGet(t.Context(), job.Job.ID)
			return err == nil && row.State == rivertype.JobStateCompleted
		}, 15*time.Second, 20*time.Millisecond)
	}
}

func usageBillingWorld(t *testing.T, enabled, manual bool) (*authorityWorld, *nmiJournal, billing.CustomerID) {
	t.Helper()
	a, p := newNMIWorld(t, func(w *world) {
		w.clock.Advance(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC).Sub(w.clock.Now()))
		w.cfg.UsageBillingEnabled = enabled
		m := w.cfg.Merchants["alpha"]
		m.UsageBilling = &billing.UsageBillingSettings{BillingDay: 25, Currencies: []billing.UsageBillingCurrency{{Currency: "USD", CreditLimit: 30_000_000, MinimumInvoiceAmount: 1_000_000}}}
		m.ArrearsGraceDays, m.ArrearsDelinquencyFloor = ptr(0), ptr(int64(0))
		w.cfg.Merchants["alpha"] = m
	})
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
				a.clock.Advance(35 * 24 * time.Hour)
				runUsageDue(t, fleet)
				after := oneUsageOrder(t, a, customer)
				require.Equal(t, before.ID, after.ID)
				require.Equal(t, before.Status, after.Status)
				require.Equal(t, before.Usage.AmountDue, after.Usage.AmountDue)
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
				events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
				require.NoError(t, err)
				encoded, err := json.Marshal(events.Events)
				require.NoError(t, err)
				require.Contains(t, string(encoded), "delinquency")
				p.money(t, 0, 0)
			})
		}
	})
}
