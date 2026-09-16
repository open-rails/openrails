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
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
)

// fakeStripe models the part of Stripe Invoicing the collection sequence
// touches, including idempotency-key replay: a request bearing a key Stripe
// has seen returns the stored response instead of creating another object.
type fakeStripe struct {
	mu        sync.Mutex
	responses map[string][]byte // idempotency key -> stored response
	invoices  map[string]map[string]any
	created   int
	payFails  int // remaining /pay calls answered 500
	keys      []string
}

func newFakeStripe(t *testing.T) (*fakeStripe, *httptest.Server) {
	t.Helper()
	f := &fakeStripe{responses: map[string][]byte{}, invoices: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeStripe) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		f.handleGet(w, r)
		return
	}
	_ = r.ParseForm()
	key := r.Header.Get("Idempotency-Key")
	f.keys = append(f.keys, key)
	if stored, ok := f.responses[key]; ok {
		_, _ = w.Write(stored)
		return
	}
	var body []byte
	switch {
	case r.URL.Path == "/v1/invoiceitems":
		body = []byte(`{"id":"ii_` + strings.TrimSuffix(key, ":invoice_item") + `"}`)
	case r.URL.Path == "/v1/invoices":
		f.created++
		id := fmt.Sprintf("in_%d", f.created)
		f.invoices[id] = map[string]any{"id": id, "status": "draft", "amount_paid": 0, "currency": "usd",
			"metadata": map[string]string{"openrails_collection_key": r.Form.Get("metadata[openrails_collection_key]")}}
		body, _ = json.Marshal(f.invoices[id])
	case strings.HasSuffix(r.URL.Path, "/finalize"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/invoices/"), "/finalize")
		f.invoices[id]["status"] = "open"
		body, _ = json.Marshal(f.invoices[id])
	case strings.HasSuffix(r.URL.Path, "/pay"):
		if f.payFails > 0 {
			f.payFails--
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream failure"}}`))
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/invoices/"), "/pay")
		f.invoices[id]["status"] = "paid"
		f.invoices[id]["amount_paid"] = 5
		f.invoices[id]["charge"] = "ch_" + id
		body, _ = json.Marshal(f.invoices[id])
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.responses[key] = body
	_, _ = w.Write(body)
}

func (f *fakeStripe) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/invoices" {
		data := make([]map[string]any, 0, len(f.invoices))
		for _, inv := range f.invoices {
			data = append(data, inv)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/invoices/")
	inv, ok := f.invoices[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"no such invoice"}}`))
		return
	}
	_ = json.NewEncoder(w).Encode(inv)
}

func (f *fakeStripe) keySequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
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

// TestInvoiceCollection_StripeReplaysProviderIdempotencyKey: a 5xx on the pay
// step parks the operation unknown; the executor replays the SAME idempotent
// sequence (every key rooted in the operation id), Stripe returns the objects
// it already created, and one invoice is paid exactly once.
func TestInvoiceCollection_StripeReplaysProviderIdempotencyKey(t *testing.T) {
	stripe, server := newFakeStripe(t)
	stripe.payFails = 1
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
	require.Equal(t, op.ID.String()+":invoice_item", keys[0])
	e.requireSettledOnce(t)
	inv := e.invoiceRow(t)
	require.Equal(t, "in_1", *inv.ExternalInvoiceID)
}

// TestInvoiceCollection_StripeWindowElapsedRequiresExactReceipt: past Stripe's
// idempotency-key retention no replay is attempted; the operation waits for
// the exact invoice, which must carry the operation key and be paid.
func TestInvoiceCollection_StripeWindowElapsedRequiresExactReceipt(t *testing.T) {
	stripe, server := newFakeStripe(t)
	stripe.payFails = 1
	e, plane, charger := stripeCollectionEnv(t, server)
	n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, charger, plane), 0)
	require.NoError(t, err)
	require.Zero(t, n)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)

	later := clockwork.NewFakeClockAt(time.Now().UTC().Add(24 * time.Hour))
	stale := collectionRunnerClock(e.db, charger, plane, later)
	_, err = e.pool.Exec(e.ctx, "UPDATE openrails.rail_intents SET next_attempt_at = $2 WHERE id = $1", op.ID, later.Now().Add(-time.Minute))
	require.NoError(t, err)
	_, err = stale.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	unknown := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, unknown.Status)
	require.Contains(t, *unknown.LastFailureReason, "idempotency window elapsed")
	require.Len(t, stripe.keySequence(), 4, "no replay after the window")

	// The invoice Stripe holds is still open: neither a receipt nor
	// non-execution... until the operator pays it out of band at Stripe.
	_, err = stale.Resolve(e.ctx, op.ID, intents.Resolution{ProviderReference: "in_1", Actor: "ops", Reason: "portal"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	_, err = stale.Resolve(e.ctx, op.ID, intents.Resolution{ProviderReference: "in_404", Actor: "ops", Reason: "portal"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	stripe.mu.Lock()
	stripe.invoices["in_1"]["status"] = "paid"
	stripe.invoices["in_1"]["amount_paid"] = 5
	stripe.invoices["in_1"]["charge"] = "ch_in_1"
	stripe.mu.Unlock()
	_, err = stale.Resolve(e.ctx, op.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "portal"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected, "non-execution is refused while Stripe shows the paid invoice")
	resolved, err := stale.Resolve(e.ctx, op.ID, intents.Resolution{ProviderReference: "in_1", Actor: "ops", Reason: "portal shows the paid invoice"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, resolved.Status)
	e.requireSettledOnce(t)
	var railPaymentID string
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT rail_payment_id FROM openrails.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, e.invoice).Scan(&railPaymentID))
	require.Equal(t, "ch_in_1", railPaymentID)
}

// TestInvoiceCollection_StripeRequestRejectionIsDefinitive: a 4xx from Stripe
// is a parsed refusal — no money moved — so the attempt fails and the invoice
// duns on its schedule instead of parking unknown.
func TestInvoiceCollection_StripeRequestRejectionIsDefinitive(t *testing.T) {
	_, server := newFakeStripe(t)
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"No such customer","code":"resource_missing"}}`))
	}))
	t.Cleanup(rejecting.Close)
	server.Close()
	e, plane, charger := stripeCollectionEnv(t, rejecting)
	n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, charger, plane), 0)
	require.NoError(t, err)
	require.Zero(t, n)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusFailedTerminal, op.Status)
	inv := e.invoiceRow(t)
	require.Nil(t, inv.CollectionIntentID)
	require.Equal(t, int32(1), inv.CollectionFailureCount)
	require.Equal(t, "resource_missing", *inv.LastCollectionFailureCode)
	require.Equal(t, []string{"failed"}, e.attemptStatuses(t))
}
