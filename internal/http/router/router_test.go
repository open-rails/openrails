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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

func TestMuxRegistrationAndMiddlewareChain(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next Handler) Handler {
			return func(r *request.Request) { order = append(order, name); next(r) }
		}
	}
	abort := func(Handler) Handler {
		return func(r *request.Request) { r.AbortCode(billing.CodeResourceAccessDenied, "no") }
	}
	var patterns []string
	mux := http.NewServeMux()
	root := NewMuxRecorded(mux, "/billing/v1", nil, func(p string) { patterns = append(patterns, p) })
	group := root.Group("/me", mw("group"))
	group.Handle(http.MethodGet, "/subscriptions/:id", func(r *request.Request) {
		order = append(order, "handler:"+r.Param("id"))
		r.NoContent()
	}, mw("route"))
	group.Group("/nested", mw("nested")).Handle(http.MethodPost, "", func(r *request.Request) {
		order = append(order, "unreachable")
	}, abort)
	require.Equal(t, []string{"GET /billing/v1/me/subscriptions/{id}", "POST /billing/v1/me/nested"}, patterns)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/me/subscriptions/sub_123", nil))
	require.Equal(t, http.StatusNoContent, rec.Code)
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

// A merchant-scoped route honors the OpenRails-Merchant selector in place: a
// named merchant is resolved and pinned before the route runs, an absent
// selector changes nothing, and no second path version exists.
func TestMerchantSelectorResolution(t *testing.T) {
	type seen struct {
		path     string
		merchant billing.MerchantID
		target   billingauth.Target
		resolved bool
	}
	var last *seen
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := seen{path: r.URL.Path}
		s.merchant, _ = merchant.FromContext(r.Context())
		s.target, s.resolved = merchanttarget.FromContext(r.Context())
		last = &s
		w.WriteHeader(http.StatusOK)
	})
	patterns := []string{
		"GET /billing/v1/admin/payments", "POST /billing/v1/admin/billing-import", "POST /billing/v1/app/usage-events",
		"GET /billing/v1/me/invoices/{id}", "GET /billing/account/invoices",
		"OPTIONS /billing/v1/me/invoices/{id}",
		"GET /billing/v1/merchants", "GET /billing/v1/catalog/products", "GET /billing/v1/config",
	}
	build := func(resolve func(context.Context, *http.Request) (billingauth.Target, error)) *Table {
		table := &Table{}
		for _, p := range patterns {
			table.Handle(p, probe)
		}
		ResolveMerchantSelectors(table, "/billing", resolve, "/billing/account")
		return table
	}
	target := billingauth.Target{MerchantID: billing.MerchantID(uuid.New()), MerchantSlug: "store"}
	var resolveErr error
	calls := 0
	table := build(func(context.Context, *http.Request) (billingauth.Target, error) {
		calls++
		return target, resolveErr
	})
	require.Len(t, table.Entries, len(patterns), "the selector adds no route")

	h := table.Handler()
	call := func(method, path string, header http.Header) *httptest.ResponseRecorder {
		last = nil
		r := httptest.NewRequest(method, path, nil)
		for k, v := range header {
			r.Header[http.CanonicalHeaderKey(k)] = v
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	slug := http.Header{merchant.SelectorHeader: {"store"}}

	for _, path := range []string{"/billing/v1/admin/payments", "/billing/v1/app/usage-events", "/billing/v1/me/invoices/inv_1", "/billing/account/invoices"} {
		method := http.MethodGet
		if strings.Contains(path, "/v1/app/") {
			method = http.MethodPost
		}
		rec := call(method, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, path)
		require.False(t, last.resolved, "%s: no selector, nothing pinned", path)
		require.True(t, last.merchant.IsZero())

		for _, header := range []http.Header{slug, {merchant.SelectorHeader: {"id:" + target.MerchantID.String()}}} {
			rec = call(method, path, header)
			require.Equal(t, http.StatusOK, rec.Code, path)
			require.Equal(t, path, last.path, "the proof-bound URI is never rewritten")
			require.Equal(t, target.MerchantID, last.merchant)
			require.Equal(t, target, last.target)
		}
	}
	require.Equal(t, 8, calls)

	// Routes that act on no credential's merchant never resolve a selector.
	for _, route := range []string{"GET /billing/v1/merchants", "GET /billing/v1/catalog/products", "GET /billing/v1/config", "OPTIONS /billing/v1/me/invoices/inv_1"} {
		method, path, _ := strings.Cut(route, " ")
		rec := call(method, path, slug)
		require.Equal(t, http.StatusOK, rec.Code, route)
		require.False(t, last.resolved, route)
	}
	require.Equal(t, 8, calls)

	for name, header := range map[string]http.Header{
		"repeated":     {merchant.SelectorHeader: {"store", "store"}},
		"blank":        {merchant.SelectorHeader: {" "}},
		"malformed id": {merchant.SelectorHeader: {"id:nope"}},
		"illegal slug": {merchant.SelectorHeader: {"Not A Slug"}},
	} {
		rec := call(http.MethodGet, "/billing/v1/admin/payments", header)
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
		require.Contains(t, rec.Body.String(), `"code":"merchant_selector_invalid"`, name)
		require.Nil(t, last, name)
	}
	require.Equal(t, 8, calls, "a malformed selector is refused before resolution")

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{billingauth.Refusal(billing.CodeMerchantNotFound), http.StatusNotFound, "merchant_not_found"},
		{billingauth.Refusal(billing.CodeMerchantBindingMismatch), http.StatusConflict, "merchant_binding_mismatch"},
		{billingauth.GateError{Status: http.StatusForbidden, Message: "not yours"}, http.StatusForbidden, "resource_access_denied"},
		{errors.New("db down"), http.StatusServiceUnavailable, "merchant_directory_unavailable"},
	} {
		resolveErr = tc.err
		rec := call(http.MethodGet, "/billing/v1/me/invoices/inv_1", slug)
		require.Equal(t, tc.status, rec.Code)
		require.Contains(t, rec.Body.String(), `"code":"`+tc.code+`"`)
		require.Nil(t, last)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/billing/v1/admin/payments", nil)
	r.Header.Set(merchant.SelectorHeader, "store")
	build(nil).Handler().ServeHTTP(rec, r)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "no resolver fails closed")
}
