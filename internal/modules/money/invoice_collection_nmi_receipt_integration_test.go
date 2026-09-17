//go:build integration

package money_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
)

// fakeNMIReceiptGateway scripts the three NMI reads the store-armed plane
// uses to bind an operator receipt: Direct Post sale (always an uncertain
// 421 here), the Query API order search, and the v5 exact payment read.
type fakeNMIReceiptGateway struct {
	mu sync.Mutex
	// saleForOrder is what the order-reference search returns per order id.
	saleForOrder map[string]string
	// payments is the v5 read per transaction id.
	payments map[string]map[string]any
	sends    int
	// saleOrderIDs records the orderid of every sale sent.
	saleOrderIDs []string
}

func newFakeNMIReceiptGateway(t *testing.T) (*fakeNMIReceiptGateway, *httptest.Server) {
	t.Helper()
	f := &fakeNMIReceiptGateway{saleForOrder: map[string]string{}, payments: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/payments/") {
			txn, ok := f.payments[strings.TrimPrefix(r.URL.Path, "/payments/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(txn)
			return
		}
		require.NoError(t, r.ParseForm())
		if r.Form.Get("type") == "sale" {
			f.sends++
			f.saleOrderIDs = append(f.saleOrderIDs, r.Form.Get("orderid"))
			fmt.Fprint(w, "response=3&responsetext=Communication+error&response_code=421")
			return
		}
		if txn, ok := f.saleForOrder[r.Form.Get("order_id")]; ok {
			fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, txn, r.Form.Get("order_id"))
			return
		}
		fmt.Fprint(w, `<nm_response></nm_response>`)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeNMIReceiptGateway) payment(txn, vault, amount, currency string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payments[txn] = map[string]any{
		"object": "transaction", "id": txn, "amount": amount, "currency": currency, "response": "1", "customer_vault_id": vault,
		"actions": []map[string]any{{"id": txn + "-a", "type": "sale", "amount": amount, "success": true, "response": "1"}},
	}
}

func (f *fakeNMIReceiptGateway) sentOrderIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.saleOrderIDs...)
}

func (f *fakeNMIReceiptGateway) orderSale(orderID, txn string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saleForOrder[orderID] = txn
}

type nmiReceiptEnv struct {
	collectionEnv
	gateway *fakeNMIReceiptGateway
	plane   *money.MerchantCollectionAdapterBuilder
	runner  *intents.Runner
	op      uuid.UUID
	vault   string
}

// nmiReceiptScenario runs one collection whose sale answer is uncertain and
// whose order search is empty, so only an operator receipt can settle it.
func nmiReceiptScenario(t *testing.T) nmiReceiptEnv {
	t.Helper()
	svc, dbi, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	msvc := merchantsServiceForTest(t, dbi)
	seedPSPSecrets(t, dbi, msvc, string(models.RailNMI), "gw-receipt-"+uuid.NewString()[:8], map[string]string{"security_key": "synthetic-key"})
	method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	invoiceID := seedArrearsInvoice(t, svc, ctx, payer, method)
	gateway, server := newFakeNMIReceiptGateway(t)
	plane := &money.MerchantCollectionAdapterBuilder{Config: storeCollectionTestConfig(), DB: dbi, MerchantsFn: func() *merchants.Service { return msvc },
		Endpoints: money.CollectionEndpoints{NMIDirectPostURL: server.URL, NMIQueryURL: server.URL, NMIV5BaseURL: server.URL}}
	charger := money.NewScopedCharger(dbi, nil)
	charger.SetAdapterResolver(plane)
	runner := collectionRunner(dbi, charger, plane)
	n, err := svc.ChargeOutstanding(ctx, runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	op := latestCollectionIntent(t, pool, ctx, invoiceID)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
	return nmiReceiptEnv{
		collectionEnv: collectionEnv{svc: svc, db: dbi, pool: pool, payer: payer, currency: currency, method: method, invoice: invoiceID, ctx: ctx},
		gateway:       gateway, plane: plane, runner: runner, op: op.ID, vault: "vault_" + method.String(),
	}
}

func (e nmiReceiptEnv) resolve(t *testing.T, txn string) error {
	t.Helper()
	_, err := e.runner.Resolve(e.ctx, e.op, intents.Resolution{ProviderReference: txn, Actor: "ops", Reason: "gateway portal"})
	return err
}

// verify runs the autonomous verifier over the operation once and returns
// its status afterwards.
func (e nmiReceiptEnv) verify(t *testing.T) string {
	t.Helper()
	dueNow(t, e.pool, e.ctx, e.op)
	_, err := e.runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	return latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status
}

// requireStillUnknown: the operation is unknown, nothing settled, the
// invoice still points at it, and the gateway saw no second sale.
func (e nmiReceiptEnv) requireStillUnknown(t *testing.T, why string) {
	t.Helper()
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status, why)
	require.Zero(t, e.settledPayments(t), why)
	require.Zero(t, e.owedPaymentTransfers(t), why)
	inv := e.invoiceRow(t)
	require.NotEqual(t, "paid", inv.Status, why)
	require.Equal(t, int64(0), inv.AmountPaid, why)
	require.NotNil(t, inv.CollectionIntentID, why)
	require.Equal(t, 1, e.gateway.sends, why)
}

// TestInvoiceCollection_NMIReceiptRejectsSaleOfAnotherOperation: an approved
// sale of the frozen amount on the same vault but for ANOTHER order (an
// earlier invoice's collection) is not this operation's receipt.
func TestInvoiceCollection_NMIReceiptRejectsSaleOfAnotherOperation(t *testing.T) {
	e := nmiReceiptScenario(t)
	e.gateway.payment("txn_last_months_invoice", e.vault, "0.05", "USD")
	e.gateway.orderSale("SOME-OTHER-OPERATION", "txn_last_months_invoice")

	err := e.resolve(t, "txn_last_months_invoice")
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Zero(t, e.settledPayments(t))
	require.Equal(t, 1, e.gateway.sends)
}

// TestInvoiceCollection_NMIReceiptRequiresExactReadToMatch: even the sale the
// order search returns for this operation must read back as approved, on
// the instrument's vault, for the frozen amount and currency.
func TestInvoiceCollection_NMIReceiptRequiresExactReadToMatch(t *testing.T) {
	e := nmiReceiptScenario(t)
	e.gateway.orderSale(e.op.String(), "txn_ours")

	require.ErrorIs(t, e.resolve(t, "txn_someone_elses"), intents.ErrResolutionRejected, "a different transaction than the order's sale")
	require.ErrorIs(t, e.resolve(t, "txn_ours"), intents.ErrResolutionRejected, "the exact read must exist")
	e.gateway.payment("txn_ours", "vault_other", "0.05", "USD")
	require.ErrorIs(t, e.resolve(t, "txn_ours"), intents.ErrResolutionRejected, "wrong vault")
	e.gateway.payment("txn_ours", e.vault, "0.50", "USD")
	require.ErrorIs(t, e.resolve(t, "txn_ours"), intents.ErrResolutionRejected, "wrong amount")
	e.gateway.payment("txn_ours", e.vault, "0.05", "EUR")
	require.ErrorIs(t, e.resolve(t, "txn_ours"), intents.ErrResolutionRejected, "wrong currency")
	require.Zero(t, e.settledPayments(t))

	e.gateway.payment("txn_ours", e.vault, "0.05", "USD")
	require.NoError(t, e.resolve(t, "txn_ours"))
	require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	e.requireSettledOnce(t)
	var railPaymentID string
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT rail_payment_id FROM openrails.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, e.invoice).Scan(&railPaymentID))
	require.Equal(t, "txn_ours", railPaymentID)
	require.Equal(t, 1, e.gateway.sends, "resolution never resends")
}

// TestInvoiceCollection_SettledReceiptIsUniquePerMerchant: the schema refuses
// a second settled attempt naming the same provider transaction on the same
// account (off-rail manual references stay free to repeat across customers).
func TestInvoiceCollection_SettledReceiptIsUniquePerMerchant(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	charger := &fakeCharger{}
	n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, charger, charger), 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	var railPaymentID string
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT rail_payment_id FROM openrails.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, e.invoice).Scan(&railPaymentID))
	_, err = e.pool.Exec(e.ctx, `INSERT INTO openrails.invoice_payments (id, merchant_id, customer_id, invoice_id, currency, amount, status, rail, rail_payment_id, psp_id)
		SELECT gen_random_uuid(), merchant_id, customer_id, invoice_id, currency, amount, 'settled', rail, rail_payment_id, psp_id FROM openrails.invoice_payments WHERE invoice_id = $1`, e.invoice)
	require.ErrorContains(t, err, "uq_invoice_payments_settled_rail_payment")
}

// TestInvoiceCollection_AutonomousVerifierRequiresExactReadToMatch (final
// review R1): the verifier settles from the order search's sale only through
// the SAME exact read operator resolution applies. After each operator
// rejection the verifier runs over the same facts and must keep the operation
// unknown with nothing settled; once the exact read matches it settles once.
func TestInvoiceCollection_AutonomousVerifierRequiresExactReadToMatch(t *testing.T) {
	e := nmiReceiptScenario(t)
	e.gateway.orderSale(e.op.String(), "txn_ours")

	require.ErrorIs(t, e.resolve(t, "txn_ours"), intents.ErrResolutionRejected, "the exact read must exist")
	require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
	e.requireStillUnknown(t, "missing exact read")

	e.gateway.payment("txn_ours", "vault_other", "0.05", "USD")
	require.ErrorIs(t, e.resolve(t, "txn_ours"), intents.ErrResolutionRejected, "wrong vault")
	require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
	e.requireStillUnknown(t, "wrong vault")

	e.gateway.payment("txn_ours", e.vault, "0.50", "USD")
	require.ErrorIs(t, e.resolve(t, "txn_ours"), intents.ErrResolutionRejected, "wrong amount")
	require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
	e.requireStillUnknown(t, "wrong amount")

	e.gateway.payment("txn_ours", e.vault, "0.05", "EUR")
	require.ErrorIs(t, e.resolve(t, "txn_ours"), intents.ErrResolutionRejected, "wrong currency")
	require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
	e.requireStillUnknown(t, "wrong currency")
	reason := latestCollectionIntent(t, e.pool, e.ctx, e.invoice).LastFailureReason
	require.NotNil(t, reason)
	require.Contains(t, *reason, "contradicts the frozen operation")

	e.gateway.payment("txn_ours", e.vault, "0.05", "USD")
	require.Equal(t, intents.StatusSucceeded, e.verify(t))
	e.requireSettledOnce(t)
	var railPaymentID string
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT rail_payment_id FROM openrails.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, e.invoice).Scan(&railPaymentID))
	require.Equal(t, "txn_ours", railPaymentID)
	require.Equal(t, 1, e.gateway.sends, "the verifier never resends")
}

// TestInvoiceCollection_NMIReceiptBindsCustodianHeldCardWithoutVault: a card
// held by the custodian (or#879) is charged by card data, so its sale has no
// customer vault at NMI. Both readers still bind approval, currency and
// amount through the exact read; the order reference binds the instrument.
func TestInvoiceCollection_NMIReceiptBindsCustodianHeldCardWithoutVault(t *testing.T) {
	e := nmiReceiptScenario(t)
	custodianID := dbtest.EnsureTestCustodian(e.ctx, t, e.pool, dbtest.TestMerchantID.UUID())
	t.Cleanup(func() {
		_, _ = e.pool.Exec(e.ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", e.method)
		_, _ = e.pool.Exec(e.ctx, "DELETE FROM openrails.custodians WHERE id = $1", custodianID)
	})
	_, err := e.pool.Exec(e.ctx, `UPDATE openrails.payment_methods SET custodian = 'basis_theory', custodian_id = $2, rail_customer_ref = '', rail_method_ref = 'tok_'||$1::text WHERE id = $1`, e.method, custodianID)
	require.NoError(t, err)
	e.gateway.orderSale(e.op.String(), "txn_pan")

	e.gateway.payment("txn_pan", "", "0.50", "USD")
	require.ErrorIs(t, e.resolve(t, "txn_pan"), intents.ErrResolutionRejected, "wrong amount")
	require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
	e.requireStillUnknown(t, "wrong amount")
	e.gateway.payment("txn_pan", "", "0.05", "EUR")
	require.ErrorIs(t, e.resolve(t, "txn_pan"), intents.ErrResolutionRejected, "wrong currency")
	require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
	e.requireStillUnknown(t, "wrong currency")

	e.gateway.payment("txn_pan", "", "0.05", "USD")
	require.Equal(t, intents.StatusSucceeded, e.verify(t))
	e.requireSettledOnce(t)
	require.Equal(t, 1, e.gateway.sends)
}

// TestInvoiceCollection_NMINonExecutionRefusedByContradictingSale: a sale for
// the operation's order reference that does not read back exactly is
// contradictory evidence, not proof of non-execution.
func TestInvoiceCollection_NMINonExecutionRefusedByContradictingSale(t *testing.T) {
	e := nmiReceiptScenario(t)
	e.gateway.orderSale(e.op.String(), "txn_ours")
	e.gateway.payment("txn_ours", e.vault, "0.50", "USD")
	_, err := e.runner.Resolve(e.ctx, e.op, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "portal"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.ErrorContains(t, err, "contradicts the operation")
	e.requireStillUnknown(t, "non-execution refused")
}
