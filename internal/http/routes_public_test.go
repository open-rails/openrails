package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/http/embedhttp"
)

func TestReadyVerboseReplacesHealthServices(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.registerStandaloneMetaRoutes(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/services", nil))
	require.Equal(t, http.StatusNotFound, w.Code)

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/ready?verbose=1", nil))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), `"dependencies"`)
}

// TestStandaloneCapabilitiesRoute proves the standalone surface serves
// GET /v1/capabilities (#623), advertising the full standalone route-group set.
func TestStandaloneCapabilitiesRoute(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.registerStandaloneMetaRoutes(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var caps struct {
		RouteGroups map[string]bool `json:"route_groups"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &caps), w.Body.String())
	// Standalone advertises StandaloneDefaultRouteSets — every standalone group on.
	for _, rs := range embedhttp.StandaloneDefaultRouteSets {
		require.True(t, caps.RouteGroups[string(rs)], "standalone group %s should be advertised", rs)
	}
}

// TestStandaloneRootBannerIsExact pins the "GET /{$}" root banner: the root
// pattern must not swallow unregistered paths (gin's "/" was exact-match).
func TestStandaloneRootBannerIsExact(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.registerStandaloneMetaRoutes(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"service"`)

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nope", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
}
