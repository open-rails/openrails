package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routesurface"
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
		require.Equal(t, rs != embedhttp.RouteSetMerchantConfig, caps.RouteGroups[string(rs)], "configuration routes require explicit publication: %s", rs)
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

func TestStandaloneCapabilitiesCredentialAuthority(t *testing.T) {
	for _, source := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		for _, writable := range []bool{false, true} {
			srv := &Server{cfg: &config.Config{MerchantConfigHTTP: true}, runtime: &app.Runtime{
				Config:            &config.Config{SecretBackend: source},
				RouteCapabilities: &routesurface.RuntimeCapabilities{SecretWrite: writable},
			}}
			mux := http.NewServeMux()
			srv.registerStandaloneMetaRoutes(mux)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
			require.Equal(t, http.StatusOK, w.Code)
			var caps embedhttp.Capabilities
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &caps))
			want := source == config.SecretBackendDB && writable
			require.Equal(t, want, caps.Features["provider_credential_writes"], "source=%s writable=%v", source, writable)
		}
	}
}
