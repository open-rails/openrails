package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// These tests cover the client-facing Idempotency-Key HTTP replay middleware
// (#579). Pattern follows ratelimit_http_test.go: httptest + a real
// idempotency.IdempotencyService backed by the in-memory fallback (nil Redis).

func idempotencyTestHandler(counter *atomic.Int64, status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func TestIdempotencyHTTP(t *testing.T) {
	t.Run("finalization survives request cancellation", func(t *testing.T) {
		requestCtx, cancelRequest := context.WithCancel(context.Background())
		cancelRequest()

		var finalizeErr error
		err := finalizeIdempotency(requestCtx, func(ctx context.Context) error {
			finalizeErr = ctx.Err()
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.WithinDuration(t, time.Now().Add(idempotencyFinalizeTimeout), deadline, time.Second)
			return nil
		})

		require.NoError(t, err)
		require.NoError(t, finalizeErr)
	})

	t.Run("replay: same key same body twice returns the same response, handler runs once", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(idempotencyTestHandler(&calls, http.StatusCreated, `{"id":"1"}`))

		key := uuid.NewString()
		body := `{"amount":100}`

		req1 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req1.Header.Set("Idempotency-Key", key)
		w1 := httptest.NewRecorder()
		h.ServeHTTP(w1, req1)

		req2 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req2.Header.Set("Idempotency-Key", key)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)

		require.EqualValues(t, 1, calls.Load())
		require.Equal(t, http.StatusCreated, w1.Code)
		require.Equal(t, w1.Code, w2.Code)
		require.Equal(t, w1.Body.String(), w2.Body.String())
		require.Empty(t, w1.Header().Get("Idempotent-Replayed"))
		require.Equal(t, "true", w2.Header().Get("Idempotent-Replayed"))
	})

	t.Run("conflict: same key different body returns 409, handler runs once", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(idempotencyTestHandler(&calls, http.StatusCreated, `{"id":"1"}`))

		key := uuid.NewString()

		req1 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(`{"amount":100}`))
		req1.Header.Set("Idempotency-Key", key)
		w1 := httptest.NewRecorder()
		h.ServeHTTP(w1, req1)

		req2 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(`{"amount":200}`))
		req2.Header.Set("Idempotency-Key", key)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)

		require.EqualValues(t, 1, calls.Load())
		require.Equal(t, http.StatusConflict, w2.Code)
		require.Contains(t, w2.Body.String(), `"code":"idempotency_key_reuse"`)
	})

	t.Run("passthrough: no header means no dedup, handler runs twice", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(idempotencyTestHandler(&calls, http.StatusOK, `{}`))

		body := `{"amount":100}`
		req1 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		w1 := httptest.NewRecorder()
		h.ServeHTTP(w1, req1)

		req2 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)

		require.EqualValues(t, 2, calls.Load())
	})

	t.Run("passthrough: GET with header is not mutating, handler runs twice", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(idempotencyTestHandler(&calls, http.StatusOK, `{}`))

		key := uuid.NewString()
		req1 := httptest.NewRequest(http.MethodGet, "/v1/checkout", nil)
		req1.Header.Set("Idempotency-Key", key)
		w1 := httptest.NewRecorder()
		h.ServeHTTP(w1, req1)

		req2 := httptest.NewRequest(http.MethodGet, "/v1/checkout", nil)
		req2.Header.Set("Idempotency-Key", key)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)

		require.EqualValues(t, 2, calls.Load())
	})

	t.Run("hard cut: X-Idempotency-Key is ignored, no dedup", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(idempotencyTestHandler(&calls, http.StatusCreated, `{"id":"1"}`))

		key := uuid.NewString()
		body := `{"amount":100}`

		// Only the legacy X-Idempotency-Key is set (never the canonical
		// Idempotency-Key). Post-hard-cut (#579) it is inert: the middleware sees
		// no key, so both requests run the handler with no replay.
		req1 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req1.Header.Set("X-Idempotency-Key", key)
		w1 := httptest.NewRecorder()
		h.ServeHTTP(w1, req1)

		req2 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req2.Header.Set("X-Idempotency-Key", key)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)

		require.EqualValues(t, 2, calls.Load())
		require.Empty(t, w2.Header().Get("Idempotent-Replayed"))
	})

	t.Run("5xx not cached: second call with same key re-runs the handler", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		var status atomic.Int64
		status.Store(http.StatusInternalServerError)
		h := IdempotencyHTTP(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(int(status.Load()))
		}))

		key := uuid.NewString()
		body := `{"amount":100}`

		req1 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req1.Header.Set("Idempotency-Key", key)
		w1 := httptest.NewRecorder()
		h.ServeHTTP(w1, req1)
		require.Equal(t, http.StatusInternalServerError, w1.Code)

		status.Store(http.StatusCreated)
		req2 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req2.Header.Set("Idempotency-Key", key)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)

		require.EqualValues(t, 2, calls.Load())
		require.Equal(t, http.StatusCreated, w2.Code)
	})

	t.Run("merchant scoping: same key+body under different merchants both run", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(idempotencyTestHandler(&calls, http.StatusCreated, `{"id":"1"}`))

		key := uuid.NewString()
		body := `{"amount":100}`

		merchant1 := merchant.ID(uuid.New())
		merchant2 := merchant.ID(uuid.New())

		req1 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req1.Header.Set("Idempotency-Key", key)
		req1 = req1.WithContext(merchant.WithID(req1.Context(), merchant1))
		w1 := httptest.NewRecorder()
		h.ServeHTTP(w1, req1)

		req2 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req2.Header.Set("Idempotency-Key", key)
		req2 = req2.WithContext(merchant.WithID(req2.Context(), merchant2))
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)

		require.EqualValues(t, 2, calls.Load())
	})

	t.Run("panic releases the pending key (retry re-runs, not locked)", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				panic("boom")
			}
			w.WriteHeader(http.StatusCreated)
		}))

		key := uuid.NewString()
		body := `{"amount":100}`

		// Call 1: the handler panics. The deferred Fail must release the pending
		// record (simulate RecoverHTTP catching the panic one frame out).
		req1 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req1.Header.Set("Idempotency-Key", key)
		w1 := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }()
			h.ServeHTTP(w1, req1)
		}()

		// Call 2: same key+body+path — must re-run, not stay 409-locked for 24h.
		req2 := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		req2.Header.Set("Idempotency-Key", key)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)

		require.EqualValues(t, 2, calls.Load(), "retry after a panic must re-run the handler, not 409-lock the key")
		require.Equal(t, http.StatusCreated, w2.Code)
	})

	t.Run("query string is part of the key (different query is not conflated)", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(idempotencyTestHandler(&calls, http.StatusCreated, `{"id":"1"}`))

		key := uuid.NewString()
		body := `{"amount":100}`

		req1 := httptest.NewRequest(http.MethodPost, "/v1/checkout?mode=a", strings.NewReader(body))
		req1.Header.Set("Idempotency-Key", key)
		w1 := httptest.NewRecorder()
		h.ServeHTTP(w1, req1)

		req2 := httptest.NewRequest(http.MethodPost, "/v1/checkout?mode=b", strings.NewReader(body))
		req2.Header.Set("Idempotency-Key", key)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)

		require.EqualValues(t, 2, calls.Load(), "distinct query strings are distinct operations")
		require.Empty(t, w2.Header().Get("Idempotent-Replayed"))
	})
}

// #SEC-23: the replay cache sits in the GLOBAL chain, ahead of route-level
// auth, and a hit answers the request WITHOUT invoking the handler. Two things
// must therefore hold: a 4xx is never cached, and the bucket is bound to the
// acting principal.
func TestIdempotencyHTTPAuthorizationBinding(t *testing.T) {
	t.Run("poisoned 401 does not answer a later authorized request", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		// Stands in for the route gate: no credential => 401, otherwise 201.
		h := IdempotencyHTTP(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"1"}`))
		}))

		// Keys derived from an order/cart id are guessable; the attacker
		// pre-poisons one with an unauthenticated 401.
		key := "order-4711"
		body := `{"amount":100}`

		poison := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		poison.Header.Set("Idempotency-Key", key)
		wp := httptest.NewRecorder()
		h.ServeHTTP(wp, poison)
		require.Equal(t, http.StatusUnauthorized, wp.Code)

		// The legitimate, authenticated request with the same key must EXECUTE.
		legit := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
		legit.Header.Set("Idempotency-Key", key)
		legit.Header.Set("Authorization", "Bearer real-token")
		wl := httptest.NewRecorder()
		h.ServeHTTP(wl, legit)

		require.Equal(t, http.StatusCreated, wl.Code, "an authorized request must never be answered from a poisoned cache")
		require.Empty(t, wl.Header().Get("Idempotent-Replayed"))
		require.EqualValues(t, 2, calls.Load(), "the handler (and its auth) must run")
	})

	t.Run("4xx is not cached: the same caller's retry re-runs the handler", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		var status atomic.Int64
		status.Store(http.StatusForbidden)
		h := IdempotencyHTTP(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(int(status.Load()))
		}))

		key := uuid.NewString()
		body := `{"amount":100}`
		send := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("Authorization", "Bearer same-token")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			return w
		}
		require.Equal(t, http.StatusForbidden, send().Code)
		status.Store(http.StatusCreated)
		require.Equal(t, http.StatusCreated, send().Code)
		require.EqualValues(t, 2, calls.Load())
	})

	t.Run("distinct principals do not share a bucket", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(idempotencyTestHandler(&calls, http.StatusCreated, `{"id":"1"}`))

		key := uuid.NewString()
		body := `{"amount":100}`
		send := func(subject billingauth.UserContext, cred string) {
			req := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("Authorization", cred)
			if subject.UserID != "" {
				req = req.WithContext(billingauth.SetUserContext(req.Context(), subject))
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
		send(billingauth.UserContext{UserID: uuid.NewString()}, "Bearer a")
		send(billingauth.UserContext{UserID: uuid.NewString()}, "Bearer b")
		send(billingauth.UserContext{}, "ApiKey sk_live_xyz") // API key: bound by credential digest
		send(billingauth.UserContext{}, "")                   // anonymous: its own bucket

		require.EqualValues(t, 4, calls.Load(), "each principal gets its own replay bucket")
	})

	t.Run("same principal still replays", func(t *testing.T) {
		svc := idempotency.NewIdempotencyService(nil)
		t.Cleanup(svc.Close)
		var calls atomic.Int64
		h := IdempotencyHTTP(svc)(idempotencyTestHandler(&calls, http.StatusCreated, `{"id":"1"}`))

		key := uuid.NewString()
		body := `{"amount":100}`
		uid := uuid.NewString()
		send := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader(body))
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("Authorization", "Bearer token-v1")
			req = req.WithContext(billingauth.SetUserContext(req.Context(), billingauth.UserContext{UserID: uid}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			return w
		}
		require.Equal(t, http.StatusCreated, send().Code)
		w2 := send()
		require.Equal(t, "true", w2.Header().Get("Idempotent-Replayed"))
		require.EqualValues(t, 1, calls.Load(), "a genuine retry from the same principal still replays")
	})
}
