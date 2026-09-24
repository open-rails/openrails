package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMuxRegistrationAndMiddlewareChain(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next Handler) Handler {
			return func(r *request.Request) { order = append(order, name); next(r) }
		}
	}
	abort := func(Handler) Handler {
		return func(r *request.Request) { r.AbortJSON(http.StatusForbidden, "no") }
	}
	var patterns []string
	mux := http.NewServeMux()
	root := NewMuxRecorded(mux, "/billing/v1", nil, func(p string) { patterns = append(patterns, p) })
	group := root.Group("/me", mw("group"))
	group.Handle(http.MethodGet, "/subscriptions/:id", func(r *request.Request) {
		order = append(order, "handler:"+r.Param("id"))
		r.SuccessJSONMessage("ok")
	}, mw("route"))
	group.Group("/nested", mw("nested")).Handle(http.MethodPost, "", func(r *request.Request) {
		order = append(order, "unreachable")
	}, abort)
	require.Equal(t, []string{"GET /billing/v1/me/subscriptions/{id}", "POST /billing/v1/me/nested"}, patterns)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/me/subscriptions/sub_123", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"group", "route", "handler:sub_123"}, order)

	order = nil
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/billing/v1/me/nested", nil))
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, []string{"group", "nested"}, order, "an aborting middleware stops the chain")
}

func TestTableWrapRetainsChainAndAddsBrowserPreflight(t *testing.T) {
	require.Panics(t, func() { (&Table{}).Handle("/no-method", http.NotFoundHandler()) })
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	table := &Table{}
	NewMux(table, "", nil).Handle(http.MethodGet, "/browser/:id", func(r *request.Request) { r.Status(http.StatusOK) })
	table.Handle("GET /admin", ok)
	table.Handle("POST /explicit", ok)
	table.Handle("OPTIONS /explicit", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	table.Entries[0].Browser = true
	table.Entries[2].Browser = true
	table.Wrap(func(e Entry) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Wrapped", e.Method+" "+e.Path)
			e.Handler.ServeHTTP(w, r)
		})
	})
	var got []string
	for _, e := range table.Entries {
		got = append(got, e.Method+" "+e.Path)
	}
	require.ElementsMatch(t, []string{"GET /browser/{id}", "GET /admin", "POST /explicit", "OPTIONS /explicit", "OPTIONS /browser/{id}"}, got)

	h := table.Handler()
	for _, tc := range []struct {
		method, path, wrapped string
		status                int
	}{
		{http.MethodGet, "/browser/7", "GET /browser/{id}", http.StatusOK},
		{http.MethodOptions, "/browser/7", "GET /browser/{id}", http.StatusOK},
		{http.MethodOptions, "/explicit", "OPTIONS /explicit", http.StatusNoContent},
		{http.MethodOptions, "/admin", "", http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, tc.status, rec.Code, tc.method+" "+tc.path)
		require.Equal(t, tc.wrapped, rec.Header().Get("X-Wrapped"), tc.method+" "+tc.path)
	}
}

// v2 selector routes are real registrations (never a URI rewrite) that demand
// an explicit merchant selector and pin exactly the resolved target.
func TestMerchantSelectorRoutes(t *testing.T) {
	type seen struct {
		path     string
		merchant merchant.ID
		target   billingauth.Target
	}
	var last *seen
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := seen{path: r.URL.Path}
		s.merchant, _ = merchant.FromContext(r.Context())
		s.target, _ = merchanttarget.FromContext(r.Context())
		last = &s
		w.WriteHeader(http.StatusOK)
	})
	build := func(resolve func(context.Context, *http.Request) (billingauth.Target, error)) *Table {
		table := &Table{}
		for _, p := range []string{
			"GET /billing/v1/merchant/payments", "GET /billing/v1/catalog", "POST /billing/v1/import/billing",
			"GET /billing/v1/me/invoices/{id}", "POST /billing/v1/me/subscriptions/{id}/retry-now",
			"GET /billing/v1/me/balance", "GET /billing/v1/merchantx", "GET /billing/v1/capabilities",
		} {
			table.Handle(p, probe)
		}
		AddMerchantSelectorRoutes(table, "/billing", resolve)
		return table
	}
	target := billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}
	var resolveErr error
	calls := 0
	table := build(func(context.Context, *http.Request) (billingauth.Target, error) {
		calls++
		return target, resolveErr
	})
	var v2 []string
	for _, e := range table.Entries {
		if strings.HasPrefix(e.Path, "/billing/v2/") {
			v2 = append(v2, e.Method+" "+e.Path)
		}
	}
	require.ElementsMatch(t, []string{
		"GET /billing/v2/merchant/payments", "GET /billing/v2/catalog", "POST /billing/v2/import/billing",
		"GET /billing/v2/me/invoices/{id}", "POST /billing/v2/me/subscriptions/{id}/retry-now",
	}, v2, "browser-only /me routes and look-alike prefixes are not duplicated")
	require.Len(t, table.Entries, 8+len(v2))

	h := table.Handler()
	call := func(method, path string, header map[string]string) *httptest.ResponseRecorder {
		last = nil
		r := httptest.NewRequest(method, path, nil)
		for k, v := range header {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	rec := call(http.MethodGet, "/billing/v2/merchant/payments", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "merchant_selector_required")
	require.Nil(t, last)
	require.Zero(t, calls)

	for _, header := range []map[string]string{{merchant.SlugHeader: "store"}, {merchant.BindingHeader: target.MerchantID.String()}} {
		rec = call(http.MethodGet, "/billing/v2/merchant/payments", header)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "/billing/v2/merchant/payments", last.path, "the proof-bound URI is never rewritten")
		require.Equal(t, target.MerchantID, last.merchant)
		require.Equal(t, target, last.target)
	}
	rec = call(http.MethodGet, "/billing/v1/merchant/payments", nil)
	require.Equal(t, http.StatusOK, rec.Code, "v1 keeps its original registration")
	require.True(t, last.merchant.IsZero(), "v1 is not pinned by the selector wrapper")

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{billingauth.GateError{Status: http.StatusForbidden, Message: "not yours"}, http.StatusForbidden, "merchant_selection_invalid"},
		{billingauth.GateError{Status: http.StatusNotFound, Message: "unknown"}, http.StatusNotFound, "merchant_selection_invalid"},
		{errors.New("db down"), http.StatusServiceUnavailable, "merchant_directory_unavailable"},
	} {
		resolveErr = tc.err
		rec = call(http.MethodGet, "/billing/v2/me/invoices/inv_1", map[string]string{merchant.SlugHeader: "store"})
		require.Equal(t, tc.status, rec.Code)
		require.Contains(t, rec.Body.String(), tc.code)
		require.Nil(t, last)
	}

	rec = httptest.NewRecorder()
	build(nil).Handler().ServeHTTP(rec, func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/billing/v2/catalog", nil)
		r.Header.Set(merchant.SlugHeader, "store")
		return r
	}())
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "no resolver fails closed")
}
