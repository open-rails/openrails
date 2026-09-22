package openrailshttp

import (
	"github.com/go-chi/chi/v5"
	"github.com/open-rails/openrails/embed"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnchoredBundleRequiresExplicitRoot(t *testing.T) {
	b := &Bundle{rootOnly: true, routes: []embed.HTTPRoute{{Method: "GET", Path: "/admin/{asset...}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/admin/js/site.js?q=raw", r.RequestURI)
		w.WriteHeader(204)
	})}}}
	mux := http.NewServeMux()
	require.ErrorContains(t, b.Mount(mux, "/outer"), "root")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/outer/admin/a", nil))
	require.Equal(t, 404, w.Code)
	require.NoError(t, b.Mount(mux))
	chiRoot := chi.NewRouter()
	require.ErrorContains(t, b.Mount(chiRoot), "MountRoot")
	require.Empty(t, chiRoot.Routes())
	require.NoError(t, b.MountRoot(chiRoot))
	for _, target := range []http.Handler{mux, chiRoot} {
		for _, method := range []string{"GET", "HEAD"} {
			w := httptest.NewRecorder()
			target.ServeHTTP(w, httptest.NewRequest(method, "/admin/js/site.js?q=raw", nil))
			require.Equal(t, 204, w.Code)
		}
	}
}
