//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// The date scan and the threshold wakeup may race across replicas. They still
// claim each accrual once and submit one provider collection for its invoice.
func TestUsageBillingThresholdAndDateShareOneCollection(t *testing.T) {
	f := startFleet(t, 2, false, nil, func(w *world) {
		w.settings.UsageBilling = &billing.UsageBillingSettings{BillingDay: 13, Currencies: []billing.UsageBillingCurrency{{Currency: "USD", CreditLimit: 100_000_000, MinimumInvoiceAmount: 1_000_000}}}
	})
	a, b := f.replicas[0], f.replicas[1]
	c := a.newCustomer()
	method := c.saveCard("nmi", visa)
	initial := newNMIInvoice(f, c)
	a.refreshProviders()
	a.settleCollectionScans()
	answer := payNMIInvoice(t.Context(), a, c, initial, method, "usage-billing-agreement")
	require.NoError(t, answer.err)
	require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, answer.status)
	f.settle()
	c.must(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": method})
	anchor := time.Date(2000, 1, 13, 0, 0, 0, 0, time.UTC)
	saved, err := a.client[remote].UpdateCustomer(t.Context(), c.cid(), billing.UpdateCustomerParams{
		CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 100_000_000}},
		UsageBilling: []billing.UsageBillingAccountUpdate{{Currency: "USD", BillingAnchor: catalog.Value(anchor)}},
	})
	require.NoError(t, err)
	require.Len(t, saved.Settings.UsageBilling, 1)
	// Debt98 plus a new5-unit request cannot fit the100-unit limit. The
	// refusal must commit a wakeup for the existing98, not roll it back.
	_, err = recordUsage(t.Context(), a.client[remote], billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "invoice-safety", Dimensions: map[string]int64{"units": 1}, Amount: new(int64(98_000_000)), Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	expires := a.clock.Now().Add(time.Hour)
	oversized, err := a.client[remote].Admit(t.Context(), []billing.AdmitParams{{CustomerID: c.cid(), Invoker: c.id, InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 101_000_000, RequestID: uuid.NewString(), ExpiresAt: &expires}})
	require.NoError(t, err)
	require.Len(t, oversized, 1)
	require.False(t, oversized[0].Allowed())
	var due bool
	require.NoError(t, a.pool.QueryRow(t.Context(), a.q(`SELECT usage_collection_due FROM billing.money_settings WHERE customer_id=$1 AND currency='USD'`), c.cid().UUID()).Scan(&due))
	require.False(t, due, "a request above full credit capacity must not trigger early collection")
	decision, err := a.client[remote].Admit(t.Context(), []billing.AdmitParams{{CustomerID: c.cid(), Invoker: c.id, InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 5_000_000, RequestID: uuid.NewString(), ExpiresAt: &expires}})
	require.NoError(t, err)
	require.Len(t, decision, 1)
	require.NotNil(t, decision[0].Admission, "%+v", decision[0])
	require.False(t, decision[0].Allowed(), "%+v", decision[0])
	a.settleCollectionScans()
	f.settle()
	preemptive, err := a.client[remote].ListOrders(t.Context(), billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: c.cid()})
	require.NoError(t, err)
	require.Len(t, preemptive.Items, 2)
	for _, order := range preemptive.Items {
		if order.ID != initial {
			requireInvoicePaidOnce(f, order.ID, 2, 2, 98_000_000, 0)
		}
	}
	next := *saved.Settings.UsageBilling[0].NextBillAt
	f.advance(next.Add(time.Minute).Sub(a.clock.Now()))
	a.refreshProviders()
	a.settleCollectionScans()
	hold := f.hold("nmi", submission("nmi"), false)
	_, err = recordUsage(t.Context(), a.client[remote], billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "invoice-safety", Dimensions: map[string]int64{"units": 1}, Amount: new(int64(100_000_000)), Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	hold.wait()
	concurrent, err := b.jobs.Insert(t.Context(), riverjobs.UsageOrderArgs{Automatic: true}, &river.InsertOpts{Queue: b.replica.queue})
	require.NoError(t, err)
	hold.release()
	b.waitJob(concurrent.Job.ID)
	f.settle()
	orders, err := a.client[remote].ListOrders(t.Context(), billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: c.cid()})
	require.NoError(t, err)
	require.Len(t, orders.Items, 3)
	for _, order := range orders.Items {
		if order.ID != initial && order.Total == 100_000_000 {
			requireInvoicePaidOnce(f, order.ID, 3, 3, 100_000_000, 0)
		}
	}
	current, err := a.client[remote].GetCustomer(t.Context(), c.cid())
	require.NoError(t, err)
	require.Equal(t, anchor, *current.Settings.UsageBilling[0].BillingAnchor)
	require.True(t, current.Settings.UsageBilling[0].NextBillAt.After(next), "fixed monthly date advances once without shifting the anchor")
	// A decline keeps the debt on the ledger and prevents another credit
	// admission even though the saved default and mandate still exist.
	a.nmi.SetDecline(visa.Last4, "202")
	defer a.nmi.SetDecline(visa.Last4, "")
	_, err = recordUsage(t.Context(), a.client[remote], billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "invoice-safety", Dimensions: map[string]int64{"units": 1}, Amount: new(int64(100_000_000)), Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	a.settleCollectionScans()
	f.settle()
	expires = a.clock.Now().Add(time.Hour)
	blocked, err := a.client[remote].Admit(t.Context(), []billing.AdmitParams{{CustomerID: c.cid(), Invoker: c.id, InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 1, RequestID: uuid.NewString(), ExpiresAt: &expires}})
	require.NoError(t, err)
	require.Len(t, blocked, 1)
	require.False(t, blocked[0].Allowed())
	balance, err := a.client[remote].GetBalance(t.Context(), c.cid(), "USD")
	require.NoError(t, err)
	require.EqualValues(t, 100_000_000, balance.OwedAmount)
}
