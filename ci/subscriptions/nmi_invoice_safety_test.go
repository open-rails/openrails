//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

const nmiInvoiceAmount = int64(100_000_000)

type collectInvoicePass struct {
	Collect bool `json:"collect"`
}

func (collectInvoicePass) Kind() string { return "openrails.invoice" }

func invoicePasses(f *fleet, collect bool) []pass {
	f.t.Helper()
	var pending []pass
	for _, r := range f.live() {
		job, err := r.jobs.Insert(f.t.Context(), collectInvoicePass{Collect: collect}, &river.InsertOpts{Queue: r.replica.queue})
		require.NoError(f.t, err)
		pending = append(pending, pass{r: r, id: job.Job.ID})
	}
	return pending
}

// Usage enters through the merchant API and the ordinary invoice worker closes
// it. Provider and financial tables are observed, never fabricated or edited.
func newNMIInvoice(f *fleet, c *customer) billing.InvoiceID {
	f.t.Helper()
	client := f.any().client[remote]
	_, err := client.SetCreditLimit(f.t.Context(), c.cid(), billing.SetCreditLimitParams{Currency: "USD", Amount: 3 * nmiInvoiceAmount})
	require.NoError(f.t, err)
	_, err = client.RecordUsage(f.t.Context(), billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "invoice-safety", Amount: nmiInvoiceAmount, Source: "test", SourceID: uuid.NewString()})
	require.NoError(f.t, err)
	f.advance(time.Minute)
	f.awaitPasses(invoicePasses(f, false))
	list, err := client.ListInvoices(f.t.Context(), billing.InvoiceListParams{CustomerID: c.cid()})
	require.NoError(f.t, err)
	var selected billing.InvoiceID
	for _, invoice := range list.Items {
		if invoice.AmountDue > 0 {
			require.True(f.t, selected.IsZero())
			selected = invoice.ID
			require.Equal(f.t, nmiInvoiceAmount, invoice.AmountDue)
		}
	}
	require.False(f.t, selected.IsZero())
	return selected
}

type invoiceAnswer struct {
	status int
	body   []byte
	err    error
}

func payNMIInvoice(ctx context.Context, r *world, c *customer, invoice billing.InvoiceID, method, key string) invoiceAnswer {
	raw, _ := json.Marshal(billing.PayInvoiceParams{PaymentMethodID: pmid(method)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.server.URL+mountPrefix+"/v1/me/invoices/"+invoice.String()+"/pay-now", bytes.NewReader(raw))
	if err != nil {
		return invoiceAnswer{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return invoiceAnswer{err: err}
	}
	body, readErr := io.ReadAll(res.Body)
	closeErr := res.Body.Close()
	if readErr != nil {
		return invoiceAnswer{err: readErr}
	}
	if closeErr != nil {
		return invoiceAnswer{err: closeErr}
	}
	return invoiceAnswer{status: res.StatusCode, body: body}
}

func requireInvoicePaidOnce(f *fleet, id billing.InvoiceID, sales, requests int, amount int64, manual int) {
	f.t.Helper()
	invoice, err := f.any().client[remote].GetInvoice(f.t.Context(), id)
	require.NoError(f.t, err)
	require.Equal(f.t, billing.InvoicePaid, invoice.Status)
	require.Zero(f.t, invoice.AmountDue)
	require.Equal(f.t, nmiInvoiceAmount, invoice.AmountPaid)
	require.Len(f.t, f.base.nmi.ledger(""), sales)
	require.Len(f.t, f.base.nmi.Attempts(), requests, "same shared database never resends the accepted collection")
	payments, err := f.any().client[remote].ListInvoicePayments(f.t.Context(), id, billing.PageRequest{})
	require.NoError(f.t, err)
	require.Len(f.t, payments.Items, 1+manual)
	var count int
	var total int64
	require.NoError(f.t, f.base.pool.QueryRow(f.t.Context(), f.q(`SELECT count(*),COALESCE(sum(amount),0) FROM billing.ledger_transfers WHERE invoice_id=$1 AND operation='invoice_payment'`), id.UUID()).Scan(&count, &total))
	require.Equal(f.t, 1, count)
	require.Equal(f.t, amount, total)
	var operations int
	require.NoError(f.t, f.base.pool.QueryRow(f.t.Context(), f.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='invoice_collection' AND payload->>'invoice_id'=$1`), id.UUID().String()).Scan(&operations))
	require.Equal(f.t, 1, operations, "one durable provider operation owns this invoice")
	var operationID, status string
	require.NoError(f.t, f.base.pool.QueryRow(f.t.Context(), f.q(`SELECT id::text,status FROM billing.provider_intents WHERE intent_type='invoice_collection' AND payload->>'invoice_id'=$1`), id.UUID().String()).Scan(&operationID, &status))
	require.Equal(f.t, "succeeded", status)
	matched := 0
	for _, request := range f.base.nmi.Attempts() {
		if request.Get("orderid") == operationID {
			matched++
		}
	}
	require.Equal(f.t, 1, matched, "the gateway order names the one accepted invoice operation")
	for _, payment := range payments.Items {
		if payment.Rail != nil {
			require.Equal(f.t, "nmi", *payment.Rail)
			require.Equal(f.t, billing.InvoicePaymentSettled, payment.Status)
			require.Equal(f.t, amount, payment.Amount)
			require.NotNil(f.t, payment.TransactionID)
			require.Equal(f.t, f.base.nmi.ledger("")[sales-1].ID, *payment.TransactionID, "retained exact provider receipt became the local invoice payment")
		}
	}
}

func TestNMIInvoiceReplicasCollectRemainingAmountOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2)
	c := f.any().newCustomer()
	method := c.saveCard("nmi", visa)
	// The customer-present first payment establishes this card's unscheduled
	// agreement through the real adapter, allowing later automatic collection.
	first := newNMIInvoice(f, c)
	gate := f.hold("nmi", submission("nmi"), false)
	start := make(chan struct{})
	initial := make([]invoiceAnswer, 6)
	var firstRequests sync.WaitGroup
	for i := range initial {
		firstRequests.Go(func() {
			<-start
			initial[i] = payNMIInvoice(t.Context(), f.replicas[i%2], c, first, method, "establish-agreement")
		})
	}
	close(start)
	gate.wait()
	gate.release()
	firstRequests.Wait()
	var operation billing.PaymentOperationID
	for _, answer := range initial {
		require.NoError(t, answer.err)
		require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, answer.status, string(answer.body))
		var accepted billing.InvoicePayNow
		require.NoError(t, json.Unmarshal(answer.body, &accepted))
		if operation.IsZero() {
			operation = accepted.Operation.ID
		}
		require.Equal(t, operation, accepted.Operation.ID, "same customer key replays the same durable operation across replicas")
	}
	f.settle()
	requireInvoicePaidOnce(f, first, 1, 1, nmiInvoiceAmount, 0)
	invoice := newNMIInvoice(f, c)
	_, err := f.any().client[remote].CreateInvoicePayment(t.Context(), invoice, billing.CreateInvoicePaymentParams{Amount: 25_000_000, Reference: "bank-partial"})
	require.NoError(t, err)
	c.must(http.MethodPut, "/collection-payment-method", "", map[string]any{"currency": "USD", "payment_method_id": method})
	// Every process is off for five days; clocks advance while none can submit.
	for _, r := range f.replicas {
		r.stop()
	}
	f.advance(5 * 24 * time.Hour)
	for _, r := range f.replicas {
		r.start()
	}
	h := f.hold("nmi", submission("nmi"), false)
	passes := invoicePasses(f, true)
	h.wait()
	var wg sync.WaitGroup
	answers := make([]invoiceAnswer, 8)
	for i := range answers {
		wg.Go(func() { answers[i] = payNMIInvoice(t.Context(), f.replicas[i%2], c, invoice, method, uuid.NewString()) })
	}
	wg.Wait()
	for _, answer := range answers {
		require.NoError(t, answer.err)
		require.Equal(t, http.StatusConflict, answer.status, string(answer.body))
	}
	h.release()
	f.awaitPasses(passes)
	f.settle()
	requireInvoicePaidOnce(f, invoice, 2, 2, 75_000_000, 1)
	require.Equal(t, "75.00", f.base.nmi.Attempts()[1].Get("amount"), "collect only the unpaid remainder")
	f.awaitPasses(invoicePasses(f, true))
	for _, r := range f.replicas {
		replay := payNMIInvoice(t.Context(), r, c, first, method, "establish-agreement")
		require.NoError(t, replay.err)
		require.Equal(t, http.StatusOK, replay.status, string(replay.body))
		fresh := payNMIInvoice(t.Context(), r, c, invoice, method, uuid.NewString())
		require.NoError(t, fresh.err)
		require.Equal(t, http.StatusConflict, fresh.status, string(fresh.body))
	}
	requireInvoicePaidOnce(f, invoice, 2, 2, 75_000_000, 1)
}

func TestNMIInvoiceReplicasAmbiguousFiveDayRestart(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		name := "not_received"
		if committed {
			name = "committed_hidden"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, 2)
			c := f.any().newCustomer()
			method := c.saveCard("nmi", visa)
			invoice := newNMIInvoice(f, c)
			if committed {
				f.base.nmi.HideSales(1)
				f.base.nmi.DropSaleResponses(1)
			} else {
				f.base.nmi.LoseSales(1)
			}
			answer := payNMIInvoice(t.Context(), f.any(), c, invoice, method, "uncertain")
			require.NoError(t, answer.err)
			require.Equal(t, http.StatusAccepted, answer.status, string(answer.body))
			f.settle()
			var operationID string
			require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT id::text FROM billing.provider_intents WHERE intent_type='invoice_collection' AND payload->>'invoice_id'=$1 AND result_evidence ? 'submitted_at'`), invoice.UUID().String()).Scan(&operationID))
			before := len(f.base.nmi.Attempts())
			if committed {
				require.Equal(t, 1, before)
				require.Len(t, f.base.nmi.ledger(""), 1, "the gateway charged before its answer disappeared")
			} else {
				require.Zero(t, before)
				require.Equal(t, 1, f.base.nmi.Lost())
			}
			// Both replicas restart only after an offline interval much longer than
			// NMI's duplicate window. Neither absence nor read failure proves no sale.
			for _, r := range f.replicas {
				r.stop()
			}
			f.advance(5 * 24 * time.Hour)
			for _, r := range f.replicas {
				r.start()
			}
			f.base.nmi.QueryUnavailable(true)
			f.wake()
			f.base.nmi.QueryUnavailable(false)
			for range 3 {
				f.advance(time.Hour)
				f.wake()
				f.awaitPasses(invoicePasses(f, true))
				for _, r := range f.replicas {
					retry := payNMIInvoice(t.Context(), r, c, invoice, method, uuid.NewString())
					require.NoError(t, retry.err)
					require.Equal(t, http.StatusConflict, retry.status, string(retry.body))
				}
			}
			require.Len(t, f.base.nmi.Attempts(), before, "unknown outcome never authorizes a repeat sale")
			var status string
			require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT status FROM billing.provider_intents WHERE id=$1`), operationID).Scan(&status))
			require.Equal(t, "unknown_needs_verify", status)
			current, err := f.any().client[remote].GetInvoice(t.Context(), invoice)
			require.NoError(t, err)
			require.Equal(t, nmiInvoiceAmount, current.AmountDue)
			require.Zero(t, current.CollectionFailureCount, "unknown submission is not a decline or a new retry allowance")
			if !committed {
				require.Equal(t, 1, f.base.nmi.Lost(), "one ambiguous submission was lost before reaching the gateway")
				require.Empty(t, f.base.nmi.ledger(""))
				return
			}
			f.base.nmi.Reveal()
			f.advance(time.Hour)
			f.wake()
			f.settle()
			requireInvoicePaidOnce(f, invoice, 1, 1, nmiInvoiceAmount, 0)
			for _, r := range f.replicas {
				replay := payNMIInvoice(t.Context(), r, c, invoice, method, "uncertain")
				require.NoError(t, replay.err)
				require.Equal(t, http.StatusOK, replay.status, string(replay.body))
			}
			requireInvoicePaidOnce(f, invoice, 1, 1, nmiInvoiceAmount, 0)
		})
	}
}

func TestNMIInvoiceReplicaCrashAfterProviderCommit(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2)
	a := f.replicas[0]
	c := a.newCustomer()
	method := c.saveCard("nmi", visa)
	invoice := newNMIInvoice(f, c)
	h := f.hold("nmi", submission("nmi"), true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan invoiceAnswer, 1)
	go func() { finished <- payNMIInvoice(ctx, a, c, invoice, method, "crashed") }()
	require.Equal(t, a, h.wait())
	f.crash(a)
	cancel()
	h.release()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("crashed caller did not stop")
	}
	require.Eventually(t, func() bool { return len(f.base.nmi.ledger("")) == 1 }, 10*time.Second, 20*time.Millisecond)
	// The second process also goes offline. Recovery must use the durable
	// fence and exact order receipt, not a new order after five days.
	other := f.any()
	other.stop()
	f.advance(5 * 24 * time.Hour)
	other.start()
	f.revive(a)
	f.any().rescue()
	f.wake()
	f.settle()
	requireInvoicePaidOnce(f, invoice, 1, 1, nmiInvoiceAmount, 0)
	for _, r := range f.replicas {
		reply := payNMIInvoice(t.Context(), r, c, invoice, method, "crashed")
		require.NoError(t, reply.err)
		require.Equal(t, http.StatusOK, reply.status, string(reply.body))
	}
	requireInvoicePaidOnce(f, invoice, 1, 1, nmiInvoiceAmount, 0)
}

// A charge accepted while writes are disabled has no submission fence. Enabling
// normal writes after a five-day outage executes that same accepted operation.
func TestNMIInvoiceAcceptedBeforeFiveDayRestart(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2)
	c := f.any().newCustomer()
	method := c.saveCard("nmi", visa)
	invoice := newNMIInvoice(f, c)
	for _, r := range f.replicas {
		r.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly }
		f.restart(r)
	}
	answer := payNMIInvoice(t.Context(), f.any(), c, invoice, method, "accepted-readonly")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusAccepted, answer.status, string(answer.body))
	var before billing.InvoicePayNow
	require.NoError(t, json.Unmarshal(answer.body, &before))
	require.False(t, before.Operation.ID.IsZero())
	var submitted bool
	require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT COALESCE(result_evidence ? 'submitted_at',false) FROM billing.provider_intents WHERE id=$1`), before.Operation.ID.UUID()).Scan(&submitted))
	require.False(t, submitted)
	require.Empty(t, f.base.nmi.Attempts())
	for _, r := range f.replicas {
		r.stop()
		r.cfg = nil
	}
	f.advance(5 * 24 * time.Hour)
	for _, r := range f.replicas {
		r.start()
	}
	f.wake()
	f.settle()
	replay := payNMIInvoice(t.Context(), f.replicas[1], c, invoice, method, "accepted-readonly")
	require.NoError(t, replay.err)
	require.Equal(t, http.StatusOK, replay.status, string(replay.body))
	var after billing.InvoicePayNow
	require.NoError(t, json.Unmarshal(replay.body, &after))
	require.Equal(t, before.Operation.ID, after.Operation.ID)
	requireInvoicePaidOnce(f, invoice, 1, 1, nmiInvoiceAmount, 0)
}
