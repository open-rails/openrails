//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/custodymigration"
	"github.com/open-rails/openrails/internal/db/gen"
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
	queryStarted chan struct{}
	queryGate    chan struct{}
	mu           sync.Mutex
	// saleForOrder is what the order-reference search returns per order id.
	saleForOrder map[string]string
	// payments is the v5 read per transaction id.
	payments map[string]map[string]any
	sends    int
	// saleOrderIDs records the orderid of every sale sent.
	saleOrderIDs   []string
	saleAmounts    []string
	saleCurrencies []string
	// saleVaults records the customer_vault_id every sale was sent on ("" for
	// a custodian-proxied sale, which carries card data instead).
	saleVaults []string
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
			f.saleAmounts = append(f.saleAmounts, r.Form.Get("amount"))
			f.saleCurrencies = append(f.saleCurrencies, r.Form.Get("currency"))
			f.saleVaults = append(f.saleVaults, r.Form.Get("customer_vault_id"))
			if r.URL.Path == "/proxy" {
				// The custodian's detokenizing proxy forwarded the sale and the
				// gateway's answer was lost on the way back (or#879 transport).
				conn, _, herr := w.(http.Hijacker).Hijack()
				require.NoError(t, herr)
				_ = conn.Close()
				return
			}
			fmt.Fprint(w, "response=3&responsetext=Communication+error&response_code=421")
			return
		}
		if gate := f.queryGate; gate != nil {
			f.queryGate = nil
			close(f.queryStarted)
			<-gate
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
	gateway   *fakeNMIReceiptGateway
	plane     *money.MerchantCollectionAdapterBuilder
	runner    *intents.Runner
	merchants *merchants.Service
	op        uuid.UUID
	vault     string
	// custodian is the merchant's declared custodian, minted on first use and
	// dropped last; shared by value copies of the env.
	custodian *uuid.UUID
}

// newNMIReceiptEnv arms one NMI account from the merchant store, seeds a
// vaulted method and an arrears invoice due for collection, and wires the
// production collection plane at a loopback gateway. Nothing is enqueued yet.
func newNMIReceiptEnv(t *testing.T) nmiReceiptEnv {
	t.Helper()
	svc, dbi, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	msvc := merchantsServiceForTest(t, dbi)
	// Registered FIRST, so it runs LAST: a custodian can only be dropped once
	// the instrument and the PSP referencing it are gone.
	var custodian uuid.UUID
	t.Cleanup(func() {
		if custodian != uuid.Nil {
			_, _ = pool.Exec(context.WithoutCancel(ctx), `DELETE FROM openrails.custodians WHERE id = $1`, custodian)
		}
	})
	seedPSPSecrets(t, dbi, msvc, string(models.RailNMI), "gw-receipt-"+uuid.NewString()[:8], map[string]string{"security_key": "synthetic-key"})
	method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	invoiceID := seedArrearsInvoice(t, svc, ctx, payer, method)
	gateway, server := newFakeNMIReceiptGateway(t)
	plane := &money.MerchantCollectionAdapterBuilder{Config: storeCollectionTestConfig(), DB: dbi, MerchantsFn: func() *merchants.Service { return msvc },
		Endpoints: money.CollectionEndpoints{NMIDirectPostURL: server.URL, NMIQueryURL: server.URL, NMIV5BaseURL: server.URL, BTBaseURL: server.URL}}
	charger := money.NewScopedCharger(dbi, nil)
	charger.SetAdapterResolver(plane)
	return nmiReceiptEnv{
		collectionEnv: collectionEnv{svc: svc, db: dbi, pool: pool, payer: payer, currency: currency, method: method, invoice: invoiceID, ctx: ctx},
		gateway:       gateway, plane: plane, runner: collectionRunner(dbi, charger, plane),
		merchants: msvc, vault: "vault_" + method.String(), custodian: &custodian,
	}
}

// collectUncertain runs the scheduled collection; the gateway's answer is
// lost, so the operation stays unknown under its one provider identity.
func (e *nmiReceiptEnv) collectUncertain(t *testing.T) {
	t.Helper()
	n, err := e.svc.ChargeOutstanding(e.ctx, e.runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
	e.op = op.ID
}

// nmiReceiptScenario runs one collection whose sale answer is uncertain and
// whose order search is empty, so only an exact receipt can settle it.
func nmiReceiptScenario(t *testing.T) nmiReceiptEnv {
	t.Helper()
	e := newNMIReceiptEnv(t)
	e.collectUncertain(t)
	return e
}

// methodRow is the instrument as it stands right now.
func (e nmiReceiptEnv) methodRow(t *testing.T) gen.OpenrailsPaymentMethod {
	t.Helper()
	row, err := gen.New(e.pool).GetPaymentMethodByID(e.ctx, e.method)
	require.NoError(t, err)
	return row
}

// frozenInstrument is the instrument the live operation froze at enqueue.
func (e nmiReceiptEnv) frozenInstrument(t *testing.T) charge.FrozenInstrument {
	t.Helper()
	var payload intents.InvoiceCollectionPayload
	require.NoError(t, json.Unmarshal(latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Payload, &payload))
	return payload.Instrument
}

// custodianAccount declares one Basis Theory custodian for the merchant, with
// its private key in the merchant secret store, and points the instrument's
// PSP at it (the or#297 survivor account shape).
func (e nmiReceiptEnv) custodianAccount(t *testing.T) uuid.UUID {
	t.Helper()
	if *e.custodian != uuid.Nil {
		return *e.custodian
	}
	id := dbtest.EnsureTestCustodian(e.ctx, t, e.pool, dbtest.TestMerchantID.UUID())
	scope, ok, err := e.merchants.CustodianScopeByID(e.ctx, dbtest.TestMerchantID, id)
	require.NoError(t, err)
	require.True(t, ok)
	ref, err := scope.SecretRef(custodians.SecretAPIKey)
	require.NoError(t, err)
	_, err = e.merchants.Secrets().Put(e.ctx, dbtest.TestMerchantID, ref.Name, "key_private_"+id.String()[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.merchants.Secrets().Delete(context.WithoutCancel(e.ctx), dbtest.TestMerchantID, ref.Name)
	})
	_, err = e.pool.Exec(e.ctx, `UPDATE openrails.psps SET custodian_id = $2 WHERE id = (SELECT psp_id FROM openrails.payment_methods WHERE id = $1)`, e.method, id)
	require.NoError(t, err)
	*e.custodian = id
	return id
}

// holdAtCustodian moves the card to the custodian BEFORE any operation exists:
// the instrument a later collection freezes is custodian-held, so its charge
// goes through the proxy and carries no vault at the gateway (or#879).
func (e nmiReceiptEnv) holdAtCustodian(t *testing.T) {
	t.Helper()
	id := e.custodianAccount(t)
	_, err := e.pool.Exec(e.ctx, `UPDATE openrails.payment_methods SET custodian = 'basis_theory', custodian_id = $2, rail_customer_ref = '', rail_method_ref = 'tok_'||$1::text WHERE id = $1`, e.method, id)
	require.NoError(t, err)
}

// moveCustodyUnderTheOperation writes exactly what a supported or#297 remap
// writes — same PSP, original vault handle retained, custody at the custodian
// — WITHOUT going through the refusal predicate, so a receipt can be judged
// against an instrument that no longer describes the charge.
func (e nmiReceiptEnv) moveCustodyUnderTheOperation(t *testing.T) {
	t.Helper()
	id := e.custodianAccount(t)
	_, err := e.pool.Exec(e.ctx, `UPDATE openrails.payment_methods SET custodian = 'basis_theory', custodian_id = $2, rail_method_ref = 'tok_'||$1::text WHERE id = $1`, e.method, id)
	require.NoError(t, err)
	row := e.methodRow(t)
	require.Equal(t, models.CustodianBasisTheory, row.Custodian)
	require.Equal(t, e.vault, row.RailCustomerRef, "the remap retains the original vault handle")
}

// remapCustody runs the REAL or#297 migration for this instrument (apply leg)
// and returns the row verdict. The same manifest is used every call, so a
// refusal and a later success are the same operator action.
func (e nmiReceiptEnv) remapCustody(t *testing.T, token string) custodymigration.RowResult {
	t.Helper()
	id := e.custodianAccount(t)
	var custodianKey string
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT key FROM openrails.custodians WHERE id = $1`, id).Scan(&custodianKey))
	sourcePSP := e.methodRow(t).PspID
	res, err := custodymigration.Migrate(e.ctx, custodymigration.Options{
		Config: storeCollectionTestConfig(), PGXPool: e.pool, MerchantID: dbtest.TestMerchantID, Apply: true,
		Export: custodymigration.VaultExport{
			ExportedAt: time.Now().UTC(), SourceRail: string(models.RailNMI), SourcePSPID: sourcePSP, Custodian: custodianKey,
			Tokens: []custodymigration.ImportedToken{{SourceRailCustomerRef: e.vault, Token: token}},
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	return res.Rows[0]
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
// held by the custodian (or#879) is charged by card data through the proxy,
// so its sale has no customer vault at NMI. The operation freezes that
// custody BEFORE it charges, and both readers bind approval, currency and
// amount through the exact read; the order reference binds the instrument.
func TestInvoiceCollection_NMIReceiptBindsCustodianHeldCardWithoutVault(t *testing.T) {
	e := newNMIReceiptEnv(t)
	e.holdAtCustodian(t)
	e.collectUncertain(t)
	frozen := e.frozenInstrument(t)
	require.Equal(t, models.CustodianBasisTheory, frozen.Custodian)
	require.Empty(t, frozen.RailCustomerRef, "a custodian-held charge addresses no gateway vault")
	require.Equal(t, []string{""}, e.gateway.saleVaults, "the proxied sale carried card data, not a vault")
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

// TestInvoiceCollection_CustodyRemapWaitsForUnresolvedCollection is Astra's
// batch-4 sequence with the or#297 refusal predicate closed: an invoice
// collection has no subscription, so the old predicate's join could not see
// it and a supported remap moved custody out from under a SENT charge. The
// flip now waits for the operation, and the operation keeps refusing the
// wrong-vault receipt while it does.
func TestInvoiceCollection_CustodyRemapWaitsForUnresolvedCollection(t *testing.T) {
	e := nmiReceiptScenario(t)
	token := "tok_" + uuid.NewString()[:12]
	// (1) the provider holds a sale for this order that is NOT this charge.
	e.gateway.orderSale(e.op.String(), "txn_wrong_vault")
	e.gateway.payment("txn_wrong_vault", "vault_other", "0.05", "USD")
	require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
	e.requireStillUnknown(t, "a sale on another vault is not this operation's receipt")

	// (2) the supported migration is refused while the charge is unresolved.
	blocked := e.remapCustody(t, token)
	require.Equal(t, custodymigration.OutcomeBlocked, blocked.Outcome)
	require.Equal(t, custodymigration.ReasonOperationUnresolved, blocked.Reason)
	row := e.methodRow(t)
	require.Equal(t, models.CustodianPSP, row.Custodian, "a refused instrument is untouched")
	require.Equal(t, e.vault, row.RailCustomerRef)

	// (3) verification and operator resolution keep refusing.
	require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
	e.requireStillUnknown(t, "after the refused remap")
	require.ErrorIs(t, e.resolve(t, "txn_wrong_vault"), intents.ErrResolutionRejected)
	e.requireStillUnknown(t, "operator receipt refused")

	// The operation resolves from its own exact receipt...
	e.gateway.orderSale(e.op.String(), "txn_ours")
	e.gateway.payment("txn_ours", e.vault, "0.05", "USD")
	require.Equal(t, intents.StatusSucceeded, e.verify(t))
	e.requireSettledOnce(t)
	require.Equal(t, 1, e.gateway.sends, "the verifier never resends")

	// ...and the same manifest then moves the card.
	remapped := e.remapCustody(t, token)
	require.Equal(t, custodymigration.OutcomeRemapped, remapped.Outcome, "a resolved operation no longer pins the instrument")
	after := e.methodRow(t)
	require.Equal(t, models.CustodianBasisTheory, after.Custodian)
	require.Equal(t, token, after.RailMethodRef)
	require.Equal(t, e.vault, after.RailCustomerRef, "the dead vault handle stays for forensics")
	e.requireSettledOnce(t)
	require.Equal(t, 1, e.gateway.sends)
}

// TestInvoiceCollection_ReceiptJudgedAgainstFrozenInstrument: the guard above
// is defence in depth — the invariant is that a receipt is judged against the
// instrument the charge was SUBMITTED on. With the instrument moved to the
// custodian underneath the operation by any other writer (exactly what a
// remap writes: same PSP, vault handle retained), the wrong-vault receipt
// must still settle nothing, and the frozen vault's exact read must still be
// what settles it — through the verifier and through the operator alike.
func TestInvoiceCollection_ReceiptJudgedAgainstFrozenInstrument(t *testing.T) {
	for _, via := range []string{"verifier", "operator"} {
		t.Run(via, func(t *testing.T) {
			e := nmiReceiptScenario(t)
			e.gateway.orderSale(e.op.String(), "txn_wrong_vault")
			e.gateway.payment("txn_wrong_vault", "vault_other", "0.05", "USD")
			require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
			e.requireStillUnknown(t, "wrong vault before the custody move")

			e.moveCustodyUnderTheOperation(t)
			frozen := e.frozenInstrument(t)
			require.Equal(t, models.CustodianPSP, frozen.Custodian, "the operation froze PSP custody")
			require.Equal(t, e.vault, frozen.RailCustomerRef)

			require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t),
				"the vault-A submission must never settle from a vault-other receipt after a custody move")
			e.requireStillUnknown(t, "custody moved under the operation")
			require.ErrorIs(t, e.resolve(t, "txn_wrong_vault"), intents.ErrResolutionRejected)
			_, err := e.runner.Resolve(e.ctx, e.op, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "portal"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected, "contradicted evidence refuses non-execution")
			e.requireStillUnknown(t, "operator refused")

			// The charge's own sale, on the vault it was submitted on.
			e.gateway.orderSale(e.op.String(), "txn_ours")
			e.gateway.payment("txn_ours", e.vault, "0.05", "USD")
			if via == "verifier" {
				require.Equal(t, intents.StatusSucceeded, e.verify(t))
			} else {
				require.NoError(t, e.resolve(t, "txn_ours"))
				require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
			}
			e.requireSettledOnce(t)
			var railPaymentID string
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT rail_payment_id FROM openrails.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, e.invoice).Scan(&railPaymentID))
			require.Equal(t, "txn_ours", railPaymentID)
			require.Equal(t, 1, e.gateway.sends, "resolution never resends")
		})
	}
}

// TestInvoiceCollection_InstrumentChangedBeforeSubmissionIsNeverSent: a
// frozen operation that has NOT crossed its submission fence refuses to
// charge an instrument that no longer matches what it froze. Nothing reaches
// the provider, the attempt fails without a decline, and the next collection
// freezes the instrument as it now is.
func TestInvoiceCollection_InstrumentChangedBeforeSubmissionIsNeverSent(t *testing.T) {
	e := newNMIReceiptEnv(t)
	// Freeze an operation without submitting it (the account is not armed yet).
	_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, &fakeCharger{prepareFailures: 1}, nil), 0)
	require.NoError(t, err)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusPending, op.Status)
	require.Equal(t, e.vault, e.frozenInstrument(t).RailCustomerRef)

	// Even pending, the operation pins the instrument against a remap.
	blocked := e.remapCustody(t, "tok_"+uuid.NewString()[:12])
	require.Equal(t, custodymigration.OutcomeBlocked, blocked.Outcome)
	require.Equal(t, custodymigration.ReasonOperationUnresolved, blocked.Reason)

	// Another writer re-vaults the card anyway.
	_, err = e.pool.Exec(e.ctx, `UPDATE openrails.payment_methods SET rail_customer_ref = 'vault_moved' WHERE id = $1`, e.method)
	require.NoError(t, err)

	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = e.runner.RunExecuteOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedTerminal, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Zero(t, e.gateway.sends, "nothing reached the provider")
	require.Zero(t, e.settledPayments(t))
	require.Zero(t, e.owedPaymentTransfers(t))
	attempts, _, err := e.svc.ListInvoicePaymentAttempts(e.ctx, e.payer, e.invoice, 20, 0)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	require.Equal(t, "failed", attempts[0].Status)
	require.NotNil(t, attempts[0].FailureCode)
	require.Equal(t, "instrument_changed", *attempts[0].FailureCode)
	inv := e.invoiceRow(t)
	require.Nil(t, inv.CollectionIntentID, "the invoice is released")
	require.NotNil(t, inv.NextCollectionAttemptAt)

	// The next collection freezes the instrument as it now is and charges it.
	e.collectUncertain(t)
	require.Equal(t, "vault_moved", e.frozenInstrument(t).RailCustomerRef)
	require.Equal(t, 1, e.gateway.sends)
	require.Equal(t, []string{"vault_moved"}, e.gateway.saleVaults)
	require.Equal(t, []string{e.op.String()}, e.gateway.sentOrderIDs())
}
