package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/pkg/merchant"
)

// The body cap is enforced on the whole body before any handler runs, webhook
// routes included (OR2-DOS-1), so no route can mutate state on a partial read.
func TestBodyLimitHTTP(t *testing.T) {
	for _, tc := range []struct {
		path, body string
		chunked    bool
		want       int
	}{
		{"/v1/webhooks/stripe", "larger-than-eight", false, 413},
		{"/billing/v1/webhooks/stripe", "larger-than-eight", true, 413},
		{"/v1/action", `{}         `, false, 413},
		{"/v1/action", `{}         `, true, 413},
		{"/v1/checkout", "12345678", true, 200},
	} {
		var got []byte
		calls := 0
		h := BodyLimitHTTP(8)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			got, _ = io.ReadAll(r.Body)
		}))
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		if tc.chunked {
			req.ContentLength = -1
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, tc.want, w.Code, tc.path)
		if tc.want == 413 {
			require.Zero(t, calls, "refused before the handler")
			require.Contains(t, w.Body.String(), `"code":"request_body_too_large"`)
			require.NotEmpty(t, w.Header().Get("X-Request-ID"))
		} else {
			require.Equal(t, tc.body, string(got), "the handler sees the full buffered body")
		}
	}
}

type unreadable struct{ t *testing.T }

func (r unreadable) Read([]byte) (int, error) {
	r.t.Error("archive body read before authentication")
	return 0, io.EOF
}

// The billing-archive import alone gets the large archive cap, streamed
// unread to its authenticated handler; lookalike paths keep the ordinary cap.
func TestArchiveBodyLimit(t *testing.T) {
	h := BodyLimitHTTP(DefaultMaxBodyBytes)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/host/billing/v1/merchant/billing-archive", unreadable{t}))
	require.Equal(t, http.StatusUnauthorized, w.Code)

	body := bytes.Repeat([]byte("x"), 2<<20)
	h = BodyLimitHTTP(DefaultMaxBodyBytes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		require.EqualValues(t, len(body), n)
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/v1/merchant/billing-archive", 204},
		{http.MethodPost, "/host/v2/merchant/billing-archive", 204},
		{http.MethodPost, "/v1/import/billing", 413},
		{http.MethodPost, "/v1/merchant/billing-archive/other", 413},
		{http.MethodPost, "/unrelated/../v1/merchant/billing-archive", 413},
		{http.MethodPost, "//v1/merchant/billing-archive", 413},
		{http.MethodPost, "/v1/merchant/%62illing-archive", 413},
		{http.MethodGet, "/v1/merchant/billing-archive", 413},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, bytes.NewReader(body)))
		require.Equal(t, tc.want, w.Code, "%s %s", tc.method, tc.path)
	}

	called := false
	h = BodyLimitHTTP(DefaultMaxBodyBytes)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, "/v1/merchant/billing-archive", strings.NewReader("unused"))
	req.ContentLength = archivewire.MaxBytes + 1
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.False(t, called)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.Contains(t, w.Body.String(), `"code":"request_body_too_large"`)
}

// #765: bearer JWTs, never cookies, authorize requests, so the browser tier
// gets a static wildcard without credentials and everything else gets no CORS.
func TestPermissiveCORSHTTP(t *testing.T) {
	tier := NewBrowserTierRoutes()
	tier.Add("GET /v1/products")
	tier.Add("POST /v1/products") // same path again must not panic
	tier.Add("GET /v1/me/{id}")
	var nilTier *BrowserTierRoutes
	nilTier.Add("GET /x")

	for _, tc := range []struct {
		match        func(*http.Request) bool
		method, path string
		granted      bool
		want         int
	}{
		{tier.Match, http.MethodGet, "/v1/products", true, 200},
		{tier.Match, http.MethodOptions, "/v1/products", true, 204},
		{tier.Match, http.MethodOptions, "/v1/me/abc", true, 204},
		{tier.Match, http.MethodGet, "/v1/merchant/settings", false, 200},
		{tier.Match, http.MethodOptions, "/v1/merchant/settings", false, 200},
		{nilTier.Match, http.MethodGet, "/v1/products", false, 200},
		{nil, http.MethodGet, "/v1/products", false, 200},
		{AllRequests, http.MethodPost, "/anything", true, 200},
	} {
		reached := false
		h := PermissiveCORSHTTP(tc.match)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true }))
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Origin", "https://storefront.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, tc.want, w.Code, "%s %s", tc.method, tc.path)
		require.Equal(t, tc.want != 204, reached, "a preflight is answered without reaching the handler")
		require.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"))
		if tc.granted {
			require.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
			require.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "Authorization")
			require.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "X-Captcha-Required")
		} else {
			require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"), "%s %s", tc.method, tc.path)
		}
	}
}

func TestSecurityHeadersAndRecovery(t *testing.T) {
	w := httptest.NewRecorder()
	SecurityHeadersHTTP()(okHandler()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/products", nil))
	require.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	require.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	require.Contains(t, w.Header().Get("Content-Security-Policy"), "default-src 'none'")

	w = httptest.NewRecorder()
	RecoverHTTP()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "internal_error")
	require.NotContains(t, w.Body.String(), "boom")

	// net/http's own abort sentinel must keep propagating.
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		RecoverHTTP()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })).
			ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
}

func TestRequestLogHTTPCorrelatesRequestID(t *testing.T) {
	logger := log.StandardLogger()
	out, formatter := logger.Out, logger.Formatter
	t.Cleanup(func() { logger.SetOutput(out); logger.SetFormatter(formatter) })
	var buf bytes.Buffer
	logger.SetOutput(&buf)
	logger.SetFormatter(&log.JSONFormatter{DisableTimestamp: true})

	for sent, want := range map[string]string{" request-965 ": "request-965", "": "", strings.Repeat("x", 129): ""} {
		buf.Reset()
		req := httptest.NewRequest(http.MethodPost, "/v1/me/checkout", nil)
		req.Header.Set("X-Request-ID", sent)
		w := httptest.NewRecorder()
		RequestLogHTTP()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(w, req)
		id := w.Header().Get("X-Request-ID")
		if want == "" {
			require.NoError(t, uuid.Validate(id), "missing or oversized ids are replaced")
		} else {
			require.Equal(t, want, id)
		}
		require.Equal(t, id, req.Header.Get("X-Request-ID"), "handlers see the same id")
		var entry map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
		require.Equal(t, id, entry["request_id"])
		require.EqualValues(t, http.StatusNoContent, entry["status"])
	}

	buf.Reset()
	w := httptest.NewRecorder()
	RequestLogHTTP("/health/live")(okHandler()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	require.NoError(t, uuid.Validate(w.Header().Get("X-Request-ID")))
	require.Zero(t, buf.Len(), "skipped paths are not logged")
}

// #336: there is no default merchant; an unresolved merchant pins nothing so
// downstream merchant.Require fails. A resolved Host also pins the host marker.
func TestResolveMerchant(t *testing.T) {
	id := merchant.ID(uuid.New())
	byHost := func(want merchant.ID, err error) HTTPMiddleware {
		return ResolveMerchantFromHostHTTP(func(_ context.Context, host string) (merchant.ID, error) {
			if host != "shop.example" {
				return id, errors.New("wrong host")
			}
			return want, err
		})
	}
	for i, tc := range []struct {
		mw           HTTPMiddleware
		pinned, host bool
	}{
		{ResolveMerchantHTTP(StaticMerchant(id)), true, false},
		{byHost(id, nil), true, true},
		{ResolveMerchantHTTP(nil), false, false},
		{ResolveMerchantHTTP(StaticMerchant(merchant.ID{})), false, false},
		{ResolveMerchantFromHostHTTP(nil), false, false},
		{byHost(id, errors.New("ambiguous")), false, false},
		{byHost(merchant.ID{}, nil), false, false},
	} {
		tc.mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got, ok := merchant.FromContext(r.Context())
			_, host := merchant.HostMerchant(r.Context())
			require.Equal(t, tc.pinned, ok, "case %d", i)
			require.Equal(t, tc.host, host, "case %d", i)
			if ok {
				require.Equal(t, id, got)
			}
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://shop.example/v1/products", nil))
	}
}
