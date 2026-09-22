//go:build integration

package money_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// fakeStripe models the part of Stripe Invoicing the collection sequence
// touches with Stripe's own semantics: an invoice item without `invoice=`
// parks a PENDING item on the customer; creating an invoice with
// `pending_invoice_items_behavior=include` sweeps every pending item into it,
// `exclude` leaves them; an item with `invoice=` attaches to that draft;
// /pay charges the invoice's amount_due and refuses a voided invoice;
// idempotency keys replay the stored response of a completed request (a 5xx
// stores nothing).
type fakeStripe struct {
	requests    []stripeRequest
	payDecline  bool
	chargeFacts func(map[string]any)
	mu          sync.Mutex
	responses   map[string][]byte
	// statuses records a stored 4xx answer for a key (Stripe replays those).
	statuses map[string]int
	pending  []stripeItem
	invoices map[string]map[string]any
	created  int
	// failNext maps a path suffix ("invoices", "invoiceitems", "pay",
	// "finalize") to the HTTP status its next request answers.
	failNext map[string]int
	// payLostResponse pays the invoice but answers 500 once.
	payLostResponse bool
	// paid mutates the invoice Stripe reports once /pay charged it: a paid
	// invoice that is not the frozen charge.
	paid    func(inv map[string]any)
	keys    []string
	charged []int64
	deleted []string
}

type stripeRequest struct {
	path, authorization string
	form                url.Values
}

func (f *fakeStripe) requestSequence() []stripeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stripeRequest(nil), f.requests...)
}

type stripeItem struct {
	id     string
	amount int64
	key    string
}

func newFakeStripe(t *testing.T) (*fakeStripe, *httptest.Server) {
	t.Helper()
	f := &fakeStripe{responses: map[string][]byte{}, statuses: map[string]int{}, invoices: map[string]map[string]any{}, failNext: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeStripe) fail(w http.ResponseWriter, code int) {
	f.failKeyed(w, code, "")
}

// failKeyed answers an error; a 4xx is stored under the idempotency key so
// a replay returns the same refusal, as Stripe does. A 5xx stores nothing.
func (f *fakeStripe) failKeyed(w http.ResponseWriter, code int, key string) {
	body := []byte(`{"error":{"message":"The payment method must be attached to the customer","code":"resource_missing"}}`)
	if code == http.StatusPaymentRequired && f.payDecline {
		body = []byte(`{"error":{"message":"Your card was declined.","code":"card_declined","decline_code":"do_not_honor"}}`)
	}
	if code >= 500 {
		body = []byte(`{"error":{"message":"upstream failure"}}`)
	} else if key != "" {
		f.responses[key] = body
		f.statuses[key] = code
	}
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (f *fakeStripe) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		f.handleGet(w, r)
		return
	case http.MethodDelete:
		f.handleDelete(w, r)
		return
	}
	_ = r.ParseForm()
	f.requests = append(f.requests, stripeRequest{r.URL.Path, r.Header.Get("Authorization"), r.Form})
	key := r.Header.Get("Idempotency-Key")
	f.keys = append(f.keys, key)
	if stored, ok := f.responses[key]; ok {
		if code := f.statuses[key]; code != 0 {
			w.WriteHeader(code)
		}
		_, _ = w.Write(stored)
		return
	}
	var body []byte
	switch {
	case r.URL.Path == "/v1/invoiceitems":
		if code := f.failNext["invoiceitems"]; code != 0 {
			delete(f.failNext, "invoiceitems")
			f.failKeyed(w, code, key)
			return
		}
		amt, _ := strconv.ParseInt(r.Form.Get("amount"), 10, 64)
		item := stripeItem{id: "ii_" + strings.TrimSuffix(key, ":invoice_item"), amount: amt, key: r.Form.Get("metadata[openrails_collection_key]")}
		if invoiceID := r.Form.Get("invoice"); invoiceID != "" {
			inv, ok := f.invoices[invoiceID]
			if !ok || inv["status"] != "draft" {
				f.fail(w, http.StatusBadRequest)
				return
			}
			inv["amount_due"] = inv["amount_due"].(int64) + amt
		} else {
			f.pending = append(f.pending, item)
		}
		body = []byte(`{"id":"` + item.id + `"}`)
	case r.URL.Path == "/v1/invoices":
		if code := f.failNext["invoices"]; code != 0 {
			delete(f.failNext, "invoices")
			f.failKeyed(w, code, key)
			return
		}
		f.created++
		id := fmt.Sprintf("in_%d", f.created)
		var total int64
		if r.Form.Get("pending_invoice_items_behavior") == "include" {
			for _, item := range f.pending {
				total += item.amount
			}
			f.pending = nil
		}
		f.invoices[id] = map[string]any{"id": id, "customer": r.Form.Get("customer"), "default_payment_method": r.Form.Get("default_payment_method"), "status": "draft", "amount_due": total, "amount_paid": int64(0), "currency": "usd",
			"metadata": map[string]string{"openrails_collection_key": r.Form.Get("metadata[openrails_collection_key]")}}
		body, _ = json.Marshal(f.invoices[id])
	case strings.HasSuffix(r.URL.Path, "/finalize"):
		if code := f.failNext["finalize"]; code != 0 {
			delete(f.failNext, "finalize")
			f.failKeyed(w, code, key)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/invoices/"), "/finalize")
		f.invoices[id]["status"] = "open"
		body, _ = json.Marshal(f.invoices[id])
	case strings.HasSuffix(r.URL.Path, "/void"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/invoices/"), "/void")
		inv, ok := f.invoices[id]
		if !ok || inv["status"] == "paid" {
			f.fail(w, http.StatusBadRequest)
			return
		}
		inv["status"] = "void"
		body, _ = json.Marshal(inv)
	case strings.HasSuffix(r.URL.Path, "/pay"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/invoices/"), "/pay")
		inv, ok := f.invoices[id]
		if !ok || inv["status"] != "open" {
			f.fail(w, http.StatusBadRequest)
			return
		}
		if code := f.failNext["pay"]; code != 0 {
			delete(f.failNext, "pay")
			f.failKeyed(w, code, key)
			return
		}
		due := inv["amount_due"].(int64)
		inv["status"] = "paid"
		inv["amount_paid"] = due
		inv["charge"] = "ch_" + id
		f.charged = append(f.charged, due)
		if f.paid != nil {
			f.paid(inv)
		}
		if f.payLostResponse {
			f.payLostResponse = false
			f.fail(w, http.StatusInternalServerError)
			return
		}
		body, _ = json.Marshal(inv)
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.responses[key] = body
	_, _ = w.Write(body)
}

func (f *fakeStripe) handleGet(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/charges/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/charges/ch_")
		inv, ok := f.invoices[id]
		if !ok || inv["status"] != "paid" {
			w.WriteHeader(404)
			return
		}
		charge := map[string]any{"id": "ch_" + id, "invoice": id, "customer": inv["customer"], "payment_method": inv["default_payment_method"], "amount_captured": inv["amount_paid"], "currency": inv["currency"], "status": "succeeded", "paid": true, "captured": true}
		if f.chargeFacts != nil {
			f.chargeFacts(charge)
		}
		_ = json.NewEncoder(w).Encode(charge)
	case r.URL.Path == "/v1/invoices":
		data := make([]map[string]any, 0, len(f.invoices))
		for _, inv := range f.invoices {
			data = append(data, inv)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false})
	case r.URL.Path == "/v1/invoiceitems":
		data := make([]map[string]any, 0, len(f.pending))
		for _, item := range f.pending {
			data = append(data, map[string]any{"id": item.id, "amount": item.amount, "metadata": map[string]string{"openrails_collection_key": item.key}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false})
	default:
		id := strings.TrimPrefix(r.URL.Path, "/v1/invoices/")
		inv, ok := f.invoices[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"no such invoice"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(inv)
	}
}

func (f *fakeStripe) handleDelete(w http.ResponseWriter, r *http.Request) {
	f.deleted = append(f.deleted, r.URL.Path)
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/invoiceitems/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/invoiceitems/")
		kept := f.pending[:0]
		for _, item := range f.pending {
			if item.id != id {
				kept = append(kept, item)
			}
		}
		f.pending = kept
	case strings.HasPrefix(r.URL.Path, "/v1/invoices/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/invoices/")
		if inv, ok := f.invoices[id]; ok && inv["status"] == "draft" {
			delete(f.invoices, id)
		} else {
			f.fail(w, http.StatusBadRequest)
			return
		}
	}
	_, _ = w.Write([]byte(`{"deleted":true}`))
}

func (f *fakeStripe) keySequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func (f *fakeStripe) chargedAmounts() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.charged...)
}

func (f *fakeStripe) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

func (f *fakeStripe) invoiceStatus(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv, ok := f.invoices[id]
	if !ok {
		return "deleted"
	}
	return inv["status"].(string)
}

// seedPendingItem parks a pending item on the customer the way an earlier,
// unrelated integration would have.
func (f *fakeStripe) seedPendingItem(amount int64, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, stripeItem{id: "ii_foreign_" + uuid.NewString()[:8], amount: amount, key: key})
}

func stripeCollectionEnv(t *testing.T, server *httptest.Server) (collectionEnv, *money.MerchantCollectionAdapterBuilder, *money.ScopedCharger) {
	t.Helper()
	svc, dbi, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	msvc := merchantsServiceForTest(t, dbi)
	sfx := uuid.NewString()[:8]
	seedPSPSecrets(t, dbi, msvc, string(models.RailStripe), "acct_replay"+sfx, map[string]string{"secret_key": "sk_test_replay_" + sfx})
	method := seedPaymentMethodWithRailCustomerRef(t, pool, ctx, payer, string(models.RailStripe), "pm_replay_"+sfx)
	seedRailCustomer(t, pool, ctx, payer, string(models.RailStripe), "cus_replay_"+sfx)
	invoiceID := seedArrearsInvoice(t, svc, ctx, payer, method)
	plane := &money.MerchantCollectionAdapterBuilder{Config: storeCollectionTestConfig(), DB: dbi, MerchantsFn: func() *merchants.Service { return msvc }, Endpoints: money.CollectionEndpoints{StripeBaseURL: server.URL}}
	charger := money.NewScopedCharger(dbi, nil)
	charger.SetAdapterResolver(plane)
	return collectionEnv{svc: svc, db: dbi, pool: pool, payer: payer, currency: currency, method: method, invoice: invoiceID, ctx: ctx}, plane, charger
}

const frozenStripeMinor = int64(5) // seedArrearsInvoice: 50_000 native = 5 cents

func (e collectionEnv) dueAgain(t *testing.T) {
	t.Helper()
	_, err := e.pool.Exec(e.ctx, `UPDATE billing.invoices SET next_collection_attempt_at = now() - interval '1 minute' WHERE id = $1`, e.invoice)
	require.NoError(t, err)
}

// TestInvoiceCollection_StripeReplaysProviderIdempotencyKey: a 5xx on the pay
// step parks the operation unknown; the executor replays the SAME idempotent
// sequence (every key rooted in the operation id), Stripe returns the objects
// it already created, and one invoice is paid exactly once.
func TestInvoiceCollection_StripeReplaysProviderIdempotencyKey(t *testing.T) {
	stripe, server := newFakeStripe(t)
	stripe.failNext["pay"] = http.StatusInternalServerError
	e, plane, charger := stripeCollectionEnv(t, server)
	runner := collectionRunner(e.db, charger, plane)

	n, err := e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
	require.Equal(t, 1, stripe.created)

	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedRetryable, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status, "within the key window the executor replays")

	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = runner.RunExecuteOnce(e.ctx)
	require.NoError(t, err)
	final := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusSucceeded, final.Status)
	require.Equal(t, 1, stripe.created, "the replay reused Stripe's objects; no second invoice")
	keys := stripe.keySequence()
	require.Len(t, keys, 8)
	require.Equal(t, keys[:4], keys[4:], "the replay carries the identical idempotency keys")
	require.Equal(t, []string{op.ID.String() + ":invoice", op.ID.String() + ":invoice_item", op.ID.String() + ":finalize", op.ID.String() + ":pay"}, keys[:4])
	require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts())
	e.requireSettledOnce(t)
	inv := e.invoiceRow(t)
	require.Equal(t, "in_1", *inv.ExternalInvoiceID)

	requests := stripe.requestSequence()
	require.Len(t, requests, 8)
	var method string
	require.NoError(t, e.pool.QueryRow(e.ctx, "SELECT rail_method_ref FROM billing.payment_methods WHERE id = $1", e.method).Scan(&method))
	sfx := strings.TrimPrefix(method, "pm_replay_")
	for i, path := range []string{"/v1/invoices", "/v1/invoiceitems", "/v1/invoices/in_1/finalize", "/v1/invoices/in_1/pay"} {
		require.Equal(t, path, requests[i].path)
		require.Equal(t, "Bearer sk_test_replay_"+sfx, requests[i].authorization)
		require.Equal(t, requests[i], requests[i+4], "replay preserves the complete provider request")
	}
	create, item, pay := requests[0].form, requests[1].form, requests[3].form
	require.Equal(t, "cus_replay_"+sfx, create.Get("customer"))
	require.Equal(t, method, create.Get("default_payment_method"))
	require.Equal(t, "charge_automatically", create.Get("collection_method"))
	require.Equal(t, "exclude", create.Get("pending_invoice_items_behavior"))
	require.Equal(t, op.ID.String(), create.Get("metadata[openrails_collection_key]"))
	require.Equal(t, "cus_replay_"+sfx, item.Get("customer"))
	require.Equal(t, "in_1", item.Get("invoice"))
	require.Equal(t, "5", item.Get("amount"), "50,000 micros converts to five cents")
	require.Equal(t, "usd", item.Get("currency"))
	require.Equal(t, e.invoice.String(), item.Get("metadata[openrails_invoice_id]"))
	require.Equal(t, op.ID.String(), item.Get("metadata[openrails_collection_key]"))
	require.Equal(t, method, pay.Get("payment_method"))
	require.Equal(t, int64(50_000), inv.AmountPaid)
	var rail, receipt string
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT rail, rail_payment_id FROM billing.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, e.invoice).Scan(&rail, &receipt))
	require.Equal(t, string(models.RailStripe), rail)
	require.Equal(t, "ch_in_1", receipt)
	n, err = e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, requests, stripe.requestSequence(), "paid invoice generates no further POST")
}

// TestInvoiceCollection_StripeWindowElapsedRequiresExactReceipt: past Stripe's
// idempotency-key retention no replay is attempted; the operation waits for
// the exact invoice, which must carry the operation key and be paid.
func TestInvoiceCollection_StripeWindowElapsedRequiresExactReceipt(t *testing.T) {
	stripe, server := newFakeStripe(t)
	stripe.payLostResponse = true
	e, plane, charger := stripeCollectionEnv(t, server)
	n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, charger, plane), 0)
	require.NoError(t, err)
	require.Zero(t, n)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
	require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts(), "the charge landed; only its response was lost")

	later := clockwork.NewFakeClockAt(time.Now().UTC().Add(24 * time.Hour))
	stale := collectionRunnerClock(e.db, charger, plane, later)
	_, err = e.pool.Exec(e.ctx, "UPDATE billing.rail_intents SET next_attempt_at = $2 WHERE id = $1", op.ID, later.Now().Add(-time.Minute))
	require.NoError(t, err)
	_, err = stale.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	unknown := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, unknown.Status)
	require.Contains(t, *unknown.LastFailureReason, "idempotency window elapsed")
	require.Len(t, stripe.keySequence(), 4, "no replay after the window")

	_, err = stale.Resolve(e.ctx, op.ID, intents.Resolution{ProviderReference: "in_404", Actor: "ops", Reason: "portal"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	_, err = stale.Resolve(e.ctx, op.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "portal"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected, "non-execution is refused while Stripe shows the paid invoice")
	require.Equal(t, "paid", stripe.invoiceStatus("in_1"), "a paid invoice is never touched by the refusal")
	resolved, err := stale.Resolve(e.ctx, op.ID, intents.Resolution{ProviderReference: "in_1", Actor: "ops", Reason: "portal shows the paid invoice"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, resolved.Status)
	e.requireSettledOnce(t)
	var railPaymentID string
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT rail_payment_id FROM billing.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, e.invoice).Scan(&railPaymentID))
	require.Equal(t, "ch_in_1", railPaymentID)
	require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts())
}

// TestInvoiceCollection_StripeRefusalAtInvoiceCreateChargesExactlyOnce: a 4xx
// on the invoice-create step is a definitive refusal that leaves nothing at
// Stripe; the next operation charges the frozen amount exactly once.
func TestInvoiceCollection_StripeRefusalAtInvoiceCreateChargesExactlyOnce(t *testing.T) {
	stripe, server := newFakeStripe(t)
	stripe.failNext["invoices"] = http.StatusBadRequest
	e, plane, charger := stripeCollectionEnv(t, server)
	runner := collectionRunner(e.db, charger, plane)

	n, err := e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusFailedTerminal, op.Status)
	inv := e.invoiceRow(t)
	require.Nil(t, inv.CollectionIntentID)
	require.Equal(t, int32(1), inv.CollectionFailureCount)
	require.Equal(t, []string{"failed"}, e.attemptStatuses(t))
	require.Equal(t, "resource_missing", *inv.LastCollectionFailureCode)
	require.Zero(t, stripe.pendingCount(), "no invoice item is parked on the customer before its invoice exists")
	require.Zero(t, stripe.created)

	e.dueAgain(t)
	n, err = e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NotEqual(t, op.ID, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).ID)
	require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts(), "Stripe charges the frozen amount exactly once")
	e.requireSettledOnce(t)
}

// TestInvoiceCollection_StripeRefusalAfterInvoiceCreateVoidsTheInvoice: a
// decline on /pay leaves an open invoice at Stripe; the refusal voids it so
// nothing can pay it later, and the retry's own invoice charges once.
func TestInvoiceCollection_StripeRefusalAfterInvoiceCreateVoidsTheInvoice(t *testing.T) {
	stripe, server := newFakeStripe(t)
	stripe.failNext["pay"] = http.StatusPaymentRequired
	stripe.payDecline = true
	e, plane, charger := stripeCollectionEnv(t, server)
	runner := collectionRunner(e.db, charger, plane)

	n, err := e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, intents.StatusFailedTerminal, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Equal(t, "void", stripe.invoiceStatus("in_1"), "the refused operation's invoice is voided at Stripe")
	stopped := e.invoiceRow(t)
	require.Nil(t, stopped.CollectionIntentID)
	// do_not_honor stops automatic collection while preserving the open debt.
	require.Equal(t, "open", stopped.Status)
	require.Nil(t, stopped.NextCollectionAttemptAt)
	require.Equal(t, int64(50_000), stopped.AmountDue)
	attempts, total, err := e.svc.ListInvoicePaymentAttempts(e.ctx, e.payer, e.invoice, 20, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, attempts, 1)
	require.Equal(t, "failed", attempts[0].Status)
	require.Equal(t, string(models.RailStripe), *attempts[0].Rail)
	require.Equal(t, "do_not_honor", *attempts[0].FailureCode)
	require.Contains(t, *attempts[0].FailureMessage, "Your card was declined.")

	e.dueAgain(t)
	n, err = e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, "paid", stripe.invoiceStatus("in_2"))
	require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts())
	e.requireSettledOnce(t)
}

// After the key window, operator nonexecution deletes an abandoned draft or
// voids an open invoice before releasing collection for a fresh operation.
func TestInvoiceCollection_StripeNotExecutedCleansPartialExecution(t *testing.T) {
	for _, tc := range []struct{ step, before, after string }{
		{"invoiceitems", "draft", "deleted"},
		{"pay", "open", "void"},
	} {
		t.Run(tc.before, func(t *testing.T) {
			stripe, server := newFakeStripe(t)
			stripe.failNext[tc.step] = http.StatusInternalServerError
			e, plane, charger := stripeCollectionEnv(t, server)
			n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, charger, plane), 0)
			require.NoError(t, err)
			require.Zero(t, n)
			op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
			require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
			require.Equal(t, tc.before, stripe.invoiceStatus("in_1"))

			later := clockwork.NewFakeClockAt(time.Now().UTC().Add(24 * time.Hour))
			stale := collectionRunnerClock(e.db, charger, plane, later)
			resolved, err := stale.Resolve(e.ctx, op.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "no paid invoice in the dashboard"})
			require.NoError(t, err)
			require.Equal(t, intents.StatusFailedTerminal, resolved.Status)
			require.Equal(t, tc.after, stripe.invoiceStatus("in_1"), "nonexecution removes the abandoned provider invoice")

			require.Nil(t, e.invoiceRow(t).CollectionIntentID)
			e.dueAgain(t)
			n, err = e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, charger, plane), 0)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.NotEqual(t, op.ID, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).ID)
			require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts())
			e.requireSettledOnce(t)
		})
	}
}

// TestInvoiceCollection_StripeIgnoresForeignPendingItems: a pending item some
// other integration parked on the customer is neither swept into this
// operation's invoice nor deleted by it.
func TestInvoiceCollection_StripeIgnoresForeignPendingItems(t *testing.T) {
	stripe, server := newFakeStripe(t)
	stripe.seedPendingItem(700, "someone-elses-operation")
	e, plane, charger := stripeCollectionEnv(t, server)
	n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, charger, plane), 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts(), "only this operation's own line is charged")
	require.Equal(t, 1, stripe.pendingCount(), "the foreign pending item is left alone")
	e.requireSettledOnce(t)
}

// TestInvoiceCollection_StripeRefusalWithFailedCleanupStaysUnknown: a refusal
// is definitive only once Stripe holds nothing chargeable for the operation;
// if the void cannot be read back or performed the outcome stays unknown.
func TestInvoiceCollection_StripeRefusalWithFailedCleanupStaysUnknown(t *testing.T) {
	stripe, _ := newFakeStripe(t)
	stripe.failNext["pay"] = http.StatusPaymentRequired
	var cleanupBlocked atomic.Bool
	cleanupBlocked.Store(true)
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cleanupBlocked.Load() && (r.Method == http.MethodGet || strings.HasSuffix(r.URL.Path, "/void")) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream failure"}}`))
			return
		}
		stripe.handle(w, r)
	}))
	t.Cleanup(blocking.Close)
	e, plane, charger := stripeCollectionEnv(t, blocking)
	n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, charger, plane), 0)
	require.NoError(t, err)
	require.Zero(t, n)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status, "a refusal whose Stripe objects cannot be cleaned up is not definitive")
	require.Equal(t, "open", stripe.invoiceStatus("in_1"))
	require.NotNil(t, e.invoiceRow(t).CollectionIntentID)

	// Once Stripe answers again, the replay re-derives the refusal and voids.
	cleanupBlocked.Store(false)
	runner := collectionRunner(e.db, charger, plane)
	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = runner.RunExecuteOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedTerminal, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Equal(t, "void", stripe.invoiceStatus("in_1"))
	require.Empty(t, stripe.chargedAmounts())
	require.Nil(t, e.invoiceRow(t).CollectionIntentID)
}

// TestInvoiceCollection_StripePaidInvoiceMustMatchFrozenOperation (final
// review R1, Stripe side): the paid invoice the sequence returns — on first
// submission and on idempotent replay — settles only through the same exact
// match operator resolution applies: key-stamped, paid, frozen currency,
// frozen amount. A contradiction keeps the operation unknown with nothing
// settled, and the same invoice is refused as an operator receipt.
func TestInvoiceCollection_StripePaidInvoiceMustMatchFrozenOperation(t *testing.T) {
	cases := map[string]func(inv map[string]any){
		"wrong_currency": func(inv map[string]any) { inv["currency"] = "eur" },
		"wrong_amount":   func(inv map[string]any) { inv["amount_paid"] = int64(50) },
		"missing_key":    func(inv map[string]any) { inv["metadata"] = map[string]string{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			stripe, server := newFakeStripe(t)
			stripe.paid = mutate
			e, plane, charger := stripeCollectionEnv(t, server)
			runner := collectionRunner(e.db, charger, plane)

			n, err := e.svc.ChargeOutstanding(e.ctx, runner, 0)
			require.NoError(t, err)
			require.Zero(t, n)
			op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
			require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
			require.Contains(t, *op.LastFailureReason, "contradicts the frozen operation")
			requireNothingSettled := func(why string) {
				t.Helper()
				require.Zero(t, e.settledPayments(t), why)
				require.Zero(t, e.owedPaymentTransfers(t), why)
				inv := e.invoiceRow(t)
				require.NotEqual(t, "paid", inv.Status, why)
				require.NotNil(t, inv.CollectionIntentID, why)
				require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts(), why)
				require.Equal(t, 1, stripe.created, why)
			}
			requireNothingSettled("first submission")

			_, err = runner.Resolve(e.ctx, op.ID, intents.Resolution{ProviderReference: "in_1", Actor: "ops", Reason: "portal"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected, "the operator is refused the same invoice")
			_, err = runner.Resolve(e.ctx, op.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "portal"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected, "a paid invoice for the key is never non-execution")
			requireNothingSettled("operator resolution")

			dueNow(t, e.pool, e.ctx, op.ID)
			_, err = runner.RunVerifyOnce(e.ctx)
			require.NoError(t, err)
			require.Equal(t, intents.StatusFailedRetryable, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
			dueNow(t, e.pool, e.ctx, op.ID)
			_, err = runner.RunExecuteOnce(e.ctx)
			require.NoError(t, err)
			replayed := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
			require.Equal(t, intents.StatusUnknownNeedsVerify, replayed.Status, "the idempotent replay returns the same contradiction")
			require.Contains(t, *replayed.LastFailureReason, "contradicts the frozen operation")
			requireNothingSettled("idempotent replay")
			require.Len(t, stripe.keySequence(), 8, "the replay reused Stripe's objects under the same keys")
		})
	}
}

func TestInvoiceCollection_StripeReceiptBindsArmedAccountAndCapturedInstrument(t *testing.T) {
	stripe, server := newFakeStripe(t)
	stripe.payLostResponse = true
	e, plane, charger := stripeCollectionEnv(t, server)
	runner := collectionRunner(e.db, charger, plane)
	_, err := e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	operation := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, operation.Status)
	wrong := subscriptions.NewAccountStripeService(nil, operation.MerchantID, uuid.New(), "wrong-account", "sk_test_wrong")
	wrong.SetBaseURLForTest(server.URL)
	_, _, err = intents.ReadStripeCollectionReceipt(e.ctx, operation, wrong, "in_1")
	require.ErrorContains(t, err, "another provider account", "matching invoice/charge facts from another armed account cannot be relabelled")
	for _, bad := range []struct {
		field string
		value any
	}{{"customer", "cus_wrong"}, {"payment_method", "pm_wrong"}, {"amount_captured", int64(99)}, {"captured", false}, {"status", "pending"}, {"invoice", "in_someone_else"}} {
		t.Run(bad.field, func(t *testing.T) {
			stripe.mu.Lock()
			stripe.chargeFacts = func(facts map[string]any) { facts[bad.field] = bad.value }
			stripe.mu.Unlock()
			_, err := runner.Resolve(e.ctx, operation.ID, intents.Resolution{ProviderReference: "in_1", Actor: "operator", Reason: "provider receipt"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected)
			require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
		})
	}
	stripe.mu.Lock()
	stripe.chargeFacts = nil
	stripe.mu.Unlock()
	complete, err := runner.Resolve(e.ctx, operation.ID, intents.Resolution{ProviderReference: "in_1", Actor: "operator", Reason: "qualified captured charge"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, complete.Status)
	receipt, found, err := intents.LoadCollectedReceipt(complete)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "ch_in_1", receipt.TransactionID())
	var evidence map[string]any
	require.NoError(t, json.Unmarshal(complete.ResultEvidence, &evidence))
	delete(evidence["qualified_receipt"].(map[string]any)["stripe"].(map[string]any), "charge_id")
	complete.ResultEvidence, err = json.Marshal(evidence)
	require.NoError(t, err)
	_, _, err = intents.LoadCollectedReceipt(complete)
	require.Error(t, err, "a persisted invoice id is never a substitute for captured charge identity")
	require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts())
}

func TestInvoiceCollection_RetainedStripeReceiptCannotBecomeNonExecution(t *testing.T) {
	stripe, server := newFakeStripe(t)
	stripe.payLostResponse = true
	e, plane, charger := stripeCollectionEnv(t, server)
	runner := collectionRunner(e.db, charger, plane)
	_, err := e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	operation := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	receipt, found, err := plane.ReadCollectionReceipt(e.ctx, operation, "in_1")
	require.NoError(t, err)
	require.True(t, found)
	_, err = intents.NewStore(e.db).RetainCollectedReceipt(e.ctx, operation, receipt)
	require.NoError(t, err)
	// Model a crash before the safe result projection and later disappearance of
	// provider objects. Cleanup now has nothing visible to void or delete.
	operation = latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Empty(t, intents.EvidenceString(operation, "transaction_id"))
	stripe.mu.Lock()
	stripe.invoices = map[string]map[string]any{}
	stripe.pending = nil
	stripe.mu.Unlock()
	_, resolutionErr := runner.Resolve(e.ctx, operation.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "provider objects disappeared"})
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status, "a proven charge cannot be turned into failed nonexecution")
	invoice := e.invoiceRow(t)
	require.NotNil(t, invoice.CollectionIntentID, "receipt custody must keep the invoice owned, preventing a new charge")
	require.Equal(t, operation.ID, *invoice.CollectionIntentID)
	require.ErrorIs(t, resolutionErr, intents.ErrResolutionRejected)
	dueNow(t, e.pool, e.ctx, operation.ID)
	_, err = runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	e.requireSettledOnce(t)
	require.Equal(t, []int64{frozenStripeMinor}, stripe.chargedAmounts())
}
