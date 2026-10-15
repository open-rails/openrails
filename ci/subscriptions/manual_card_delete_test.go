//go:build e2e && integration

package subscriptions_test

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func nativeCardDeleteRequest(r *http.Request) bool {
	return r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/customers/")
}

func assertNoCardDeleteJobs(t *testing.T, w *world, method string) {
	t.Helper()
	var jobs int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.river_job j
        JOIN billing.provider_intents i ON i.id::text=j.args->>'intent_id'
        WHERE j.kind='openrails.provider_operation' AND i.intent_type='nmi_vault_delete'
          AND i.payload->>'payment_method_id'=$1`), pmid(method).UUID().String()).Scan(&jobs))
	require.Zero(t, jobs, "card removal never queues a provider retry")
}

func TestManualNMICardDeleteCompletesInline(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	status, out := c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
	require.Equal(t, http.StatusNoContent, status, "%v", out)
	require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), 1)
	require.Empty(t, c.must(http.MethodGet, "/payment-methods", "", nil)["data"])
	var state, marker string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status,park_reason FROM billing.payment_methods WHERE id=$1`), pmid(method).UUID()).Scan(&state, &marker))
	require.Equal(t, "removed", state)
	require.True(t, strings.HasPrefix(marker, "removed:"), "local history is a completed nonchargeable tombstone")
	assertNoCardDeleteJobs(t, w, method)
}

func TestManualNMICardDeleteFailureNeedsExplicitRetry(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	var failing atomic.Bool
	var attempts atomic.Int32
	failing.Store(true)
	w.nmi.FailRequests(func(r *http.Request) bool {
		if !nativeCardDeleteRequest(r) {
			return false
		}
		attempts.Add(1)
		return failing.Load()
	}, http.StatusServiceUnavailable, 100)
	status, out := c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
	require.Equal(t, http.StatusServiceUnavailable, status, "%v", out)
	require.EqualValues(t, 1, attempts.Load())
	assertNoCardDeleteJobs(t, w, method)
	status, out = c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
	require.Equal(t, http.StatusServiceUnavailable, status, "%v", out)
	require.EqualValues(t, 2, attempts.Load(), "an immediate explicit retry is not held by background backoff")
	failing.Store(false)
	w.advance(time.Hour)
	w.wake()
	require.EqualValues(t, 2, attempts.Load(), "workers never retry the failed user request")
	status, out = c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
	require.Equal(t, http.StatusNoContent, status, "%v", out)
	require.EqualValues(t, 3, attempts.Load())
	require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), 1, "only the explicit successful attempt reached the provider handler")
	assertNoCardDeleteJobs(t, w, method)
}

func TestManualNMICardDeleteLostResponseUsesReadback(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	var readbackUnavailable atomic.Bool
	w.nmi.Intercept(nativeCardDeleteRequest, func(_ *http.Request, serve func() *http.Response) (*http.Response, error) {
		response := serve()
		_ = response.Body.Close()
		readbackUnavailable.Store(true)
		return nil, io.ErrUnexpectedEOF
	})
	w.nmi.Intercept(func(r *http.Request) bool {
		return readbackUnavailable.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/customers/")
	}, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("readback unavailable")), Request: r}, nil
	})
	status, out := c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
	require.Equal(t, http.StatusServiceUnavailable, status, "%v", out)
	require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), 1)
	assertNoCardDeleteJobs(t, w, method)
	w.nmi.ClearIntercepts()
	w.advance(time.Hour)
	w.wake()
	require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), 1, "ambiguity schedules no background retry")
	status, out = c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
	require.Equal(t, http.StatusNoContent, status, "%v", out)
	require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), 1, "manual retry confirms the prior delete instead of repeating it")
	assertNoCardDeleteJobs(t, w, method)
}

func TestUnattemptedLegacyCardDeleteCanBeRetiredForManualRetry(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	runtime := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	ctx = billingauth.BindIdentity(ctx, billingauth.Identity{Subject: c.id, Issuer: authtest.Issuer})
	pm, err := runtime.PaymentMethodService.GetByID(ctx, pmid(method).UUID())
	require.NoError(t, err)
	store := intents.NewStore(runtime.DB)
	legacy, err := store.Enqueue(ctx, intents.EnqueueParams{
		MerchantID: w.client[embedded].MerchantID().UUID(), Provider: "nmi", IntentType: intents.TypeNMIPaymentMethodDelete,
		PspID: w.psp["nmi"].UUID(), Origin: intents.OriginUser, Actor: c.id, OriginReason: "legacy user deletion fixture",
		IdempotencyKey: intents.NMIPaymentMethodDeleteIdempotencyKey(pm.ID), NextAttemptAt: w.clock.Now(),
		Payload: intents.NMIPaymentMethodDeletePayload{UserID: c.id, PaymentMethodID: pm.ID, RailCustomerRef: pm.RailCustomerRef, RailMethodRef: pm.RailMethodRef},
	})
	require.NoError(t, err)
	job, err := w.jobs.Insert(t.Context(), intents.OperationArgs{MerchantID: legacy.MerchantID, IntentID: legacy.ID}, &river.InsertOpts{Queue: "billing"})
	require.NoError(t, err)
	w.waitJob(job.Job.ID)
	current, err := store.Get(ctx, legacy.ID)
	require.NoError(t, err)
	require.EqualValues(t, 0, current.Attempts, "even an old queued job cannot perform a user card deletion")
	var queued int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.river_job
        WHERE kind='openrails.provider_operation' AND args->>'intent_id'=$1`), legacy.ID.String()).Scan(&queued))
	require.Equal(t, 1, queued, "the completed legacy job creates no successor or retry")
	require.Empty(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil))
	require.NoError(t, store.RetireUnattemptedUserMethodDelete(ctx, legacy.ID, pm, w.clock.Now()))
	current, err = store.Get(ctx, legacy.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSuperseded, current.Status)
	status, out := c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
	require.Equal(t, http.StatusNoContent, status, "%v", out)
	require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), 1)
	var manual int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='nmi_vault_delete'
        AND payload->>'payment_method_id'=$1 AND idempotency_key LIKE 'manual:%' AND status='succeeded'`), pm.ID.String()).Scan(&manual))
	require.Equal(t, 1, manual)
}
