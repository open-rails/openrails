package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/request"
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
