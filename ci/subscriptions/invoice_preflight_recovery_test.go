//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestInvoicePreflightRecoversPaymentNewerThanBulkWindow(t *testing.T) {
	t.Parallel()
	source := newWorld(t)
	c := source.newCustomer()
	method := c.saveCard("nmi", visa)
	invoice := observedInvoice(t, source, c, 50_000_001)
	mid := source.client[embedded].MerchantID()
	source.settle()
	source.stop()
	sourceDB, err := db.NewWithPGXPool(source.pool, source.schema)
	require.NoError(t, err)
	var archive bytes.Buffer
	require.NoError(t, merchantarchive.Export(t.Context(), sourceDB, mid, &archive))
	source.start()
	answer := payNMIInvoice(t.Context(), source, c, invoice, method, "paid-after-backup")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusOK, answer.status, string(answer.body))
	source.settle()
	source.stop()
	paidAt := source.clock.Now()

	target := prepareWorldAtDSN(t, 12, providerCopyDatabase(t, dsn(t)))
	target.slug, target.auth, target.nmi, target.stripe = source.slug, source.auth, source.nmi, source.stripe
	target.clock = source.clock
	targetDB, err := db.NewWithPGXPool(target.pool, target.schema)
	require.NoError(t, err)
	directory, err := merchants.NewDirectoryService(targetDB.DataPool())
	require.NoError(t, err)
	_, _, err = directory.RegisterForRestore(t.Context(), mid, target.slug)
	require.NoError(t, err)
	_, err = merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	target.advance(time.Minute)
	target.start()
	require.True(t, paidAt.After(target.clock.Now().Add(-5*time.Minute)), "receipt is newer than the bulk catch-up safety horizon")
	retry := payNMIInvoice(t.Context(), target, c, invoice, method, "new-local-attempt")
	require.NoError(t, retry.err)
	require.Len(t, target.nmi.Attempts(), 1, "old source without preflight charges this already-paid invoice twice")
	require.Equal(t, http.StatusConflict, retry.status, string(retry.body))
	require.Contains(t, string(retry.body), "payment_not_retryable")
	paid, err := target.client[remote].GetInvoice(t.Context(), invoice)
	require.NoError(t, err)
	require.Equal(t, billing.InvoicePaid, paid.Status)
	require.Zero(t, paid.AmountDue)
	var operations, allocations int
	require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='invoice_collection'`)).Scan(&operations))
	require.Zero(t, operations, "readback did not fabricate an accepted charge or initiator")
	require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.ledger_transfers WHERE operation='invoice_payment' AND invoice_id=$1`), invoice.UUID()).Scan(&allocations))
	require.Equal(t, 1, allocations)
	require.Len(t, target.nmi.Attempts(), 1, "only the source charged; destination's fresh request recovered")
	require.Len(t, target.nmi.ledger(""), 1)
}

func TestInvoicePreflightHoldsVisibleUnknownUntilPositiveReceipt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	invoice := observedInvoice(t, w, c, 50_000_000)
	w.nmi.AddSale(nmimock.Sale{OrderID: uuid.NewString(), OrderDescription: "invoice " + invoice.UUID().String(), Vault: w.vaultOf(method), Amount: "50.00"})
	var uncertain atomic.Bool
	uncertain.Store(true)
	w.nmi.Intercept(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/query.php") }, func(_ *http.Request, serve func() *http.Response) (*http.Response, error) {
		response := serve()
		if !uncertain.Load() {
			return response, nil
		}
		raw, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		body := strings.ReplaceAll(string(raw), "<success>1</success>", "<success>0</success>")
		body = strings.ReplaceAll(body, "<response_code>100</response_code>", "<response_code>420</response_code>")
		response.Body = io.NopCloser(strings.NewReader(body))
		response.ContentLength = int64(len(body))
		return response, nil
	})
	t.Cleanup(w.nmi.ClearIntercepts)
	answer := payNMIInvoice(t.Context(), w, c, invoice, method, "visible-unknown")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusServiceUnavailable, answer.status, string(answer.body))
	require.Contains(t, string(answer.body), `"code":"service_unavailable"`)
	require.Empty(t, w.nmi.Attempts(), "visible uncertainty is not absence and never authorizes another sale")
	var operations int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='invoice_collection'`)).Scan(&operations))
	require.Zero(t, operations)
	stillDue, err := w.client[remote].GetInvoice(t.Context(), invoice)
	require.NoError(t, err)
	require.Equal(t, int64(50_000_000), stillDue.AmountDue)

	uncertain.Store(false)
	retry := payNMIInvoice(t.Context(), w, c, invoice, method, "visible-unknown")
	require.NoError(t, retry.err)
	require.Equal(t, http.StatusConflict, retry.status, string(retry.body))
	paid, err := w.client[remote].GetInvoice(t.Context(), invoice)
	require.NoError(t, err)
	require.Equal(t, billing.InvoicePaid, paid.Status)
	require.Empty(t, w.nmi.Attempts())
	require.Len(t, w.nmi.ledger(""), 1)
}

func TestInvoicePreflightCannotRetireConcurrentSubmissionFence(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	invoice := observedInvoice(t, w, c, 50_000_000)
	w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly }
	w.restart()
	answer := payNMIInvoice(t.Context(), w, c, invoice, method, "fence-race")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusAccepted, answer.status, string(answer.body))
	var accepted billing.InvoicePayNow
	require.NoError(t, json.Unmarshal(answer.body, &accepted))
	sale := w.nmi.AddSale(nmimock.Sale{OrderID: uuid.NewString(), OrderDescription: "invoice " + invoice.UUID().String(), Vault: w.vaultOf(method), Amount: "50.00"})
	gate := w.nmi.hold(newGate(func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payments/"+sale.TransactionID)
	}, false))
	t.Cleanup(func() {
		w.nmi.unhold()
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	w.cfg = nil
	w.advance(time.Minute)
	w.restart()
	result := make(chan invoiceAnswer, 1)
	go func() { result <- payNMIInvoice(t.Context(), w, c, invoice, method, "fence-race") }()
	select {
	case <-gate.arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("preflight never read the foreign receipt")
	}
	// The actual submission-fence writer wins while the earlier observer is
	// reading the provider. No fabricated receipt or financial row is inserted.
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	rt := engine.Graph(w.rt).Runtime
	store := intents.NewStore(rt.DB)
	in, err := store.Get(ctx, accepted.Operation.ID.UUID())
	require.NoError(t, err)
	_, first, err := store.BeginCollectedPayment(ctx, in, w.clock.Now())
	require.NoError(t, err)
	require.True(t, first)
	close(gate.release)
	w.nmi.unhold()
	reply := <-result
	require.NoError(t, reply.err)
	w.settle()
	current, err := store.Get(ctx, accepted.Operation.ID.UUID())
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
	require.NotEmpty(t, intents.EvidenceString(current, "submitted_at"))
	require.Empty(t, intents.EvidenceString(current, "not_executed"), "the racing fence prevents unsent completion")
	stillDue, err := w.client[remote].GetInvoice(t.Context(), invoice)
	require.NoError(t, err)
	require.Equal(t, int64(50_000_000), stillDue.AmountDue)
	var allocations int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.ledger_transfers WHERE invoice_id=$1 AND operation='invoice_payment'`), invoice.UUID()).Scan(&allocations))
	require.Zero(t, allocations)
	require.Empty(t, w.nmi.Attempts())
}

// An admitted but unfenced operation can encounter provider history before its
// first dispatch. Same-order receipts use canonical custody; foreign orders
// retire only the unsent operation and keep the actual receipt's identity.
func TestInvoicePreflightResolvesUnsentAcceptedOperation(t *testing.T) {
	t.Parallel()
	for _, sameOrder := range []bool{true, false} {
		name := "another_order"
		if sameOrder {
			name = "same_order"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			method := c.saveCard("nmi", visa)
			invoice := observedInvoice(t, w, c, 50_000_000)
			w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly }
			w.restart()
			answer := payNMIInvoice(t.Context(), w, c, invoice, method, "accepted-unsent")
			require.NoError(t, answer.err)
			require.Equal(t, http.StatusAccepted, answer.status, string(answer.body))
			var accepted billing.InvoicePayNow
			require.NoError(t, json.Unmarshal(answer.body, &accepted))
			operation := accepted.Operation.ID.UUID()
			var fenced bool
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT COALESCE(result_evidence ? 'submitted_at',false) FROM billing.provider_intents WHERE id=$1`), operation).Scan(&fenced))
			require.False(t, fenced)
			order := operation.String()
			if !sameOrder {
				order = uuid.NewString()
			}
			sale := w.nmi.AddSale(nmimock.Sale{OrderID: order, OrderDescription: "invoice " + invoice.UUID().String(), Vault: w.vaultOf(method), Amount: "50.00"})
			w.cfg = nil
			w.advance(time.Minute)
			w.restart()
			w.wake()
			w.settle()
			retry := payNMIInvoice(t.Context(), w, c, invoice, method, "accepted-unsent")
			require.NoError(t, retry.err)
			if sameOrder {
				require.Equal(t, http.StatusOK, retry.status, string(retry.body))
			} else {
				require.Equal(t, http.StatusConflict, retry.status, string(retry.body))
			}
			paid, err := w.client[remote].GetInvoice(t.Context(), invoice)
			require.NoError(t, err)
			require.Equal(t, billing.InvoicePaid, paid.Status)
			require.Zero(t, paid.AmountDue)
			var status string
			var evidence []byte
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status,result_evidence FROM billing.provider_intents WHERE id=$1`), operation).Scan(&status, &evidence))
			if sameOrder {
				require.Equal(t, "succeeded", status)
				require.Contains(t, string(evidence), sale.TransactionID)
			} else {
				require.Equal(t, "failed_terminal", status)
				require.Contains(t, string(evidence), "invoice_paid_elsewhere")
				require.NotContains(t, string(evidence), sale.TransactionID, "foreign receipt was not made this operation's charge")
			}
			require.Empty(t, w.nmi.Attempts(), "positive readback never sent another sale")
			require.Len(t, w.nmi.ledger(""), 1)
			var allocations int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.ledger_transfers WHERE operation='invoice_payment' AND invoice_id=$1`), invoice.UUID()).Scan(&allocations))
			require.Equal(t, 1, allocations)
		})
	}
}
