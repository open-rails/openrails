package server

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

func TestRegisterServiceRoutes_DisabledDoesNotMountServiceHealth(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := &Server{
		cfg:            &config.Config{},
		privateHandler: gin.New(),
	}

	srv.registerServiceRoutes()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.privateHandler.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestRegisterServiceRoutes_RequiresClientCertificate(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := &Server{
		cfg: &config.Config{
			ServiceMTLS: config.ServiceMTLSConfig{
				Enabled:           true,
				AllowedClientSANs: []string{"authkit.internal"},
			},
		},
		privateHandler: gin.New(),
	}

	srv.registerServiceRoutes()

	req := httptest.NewRequest(http.MethodGet, "/v1/users/user-1/entitlements", nil)
	req.Header.Set("X-API-KEY", "legacy-key")
	w := httptest.NewRecorder()
	srv.privateHandler.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRegisterServiceRoutes_RejectsUnconfiguredCertificateIdentity(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := &Server{
		cfg: &config.Config{
			ServiceMTLS: config.ServiceMTLSConfig{
				Enabled:           true,
				AllowedClientSANs: []string{"authkit.internal"},
			},
		},
		privateHandler: gin.New(),
	}

	srv.registerServiceRoutes()

	req := httptest.NewRequest(http.MethodGet, "/v1/users/user-1/entitlements", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{
			DNSNames: []string{"wrong.internal"},
		}},
	}
	w := httptest.NewRecorder()
	srv.privateHandler.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestRegisterServiceRoutes_RejectsInsufficientServiceScope(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := &Server{
		cfg: &config.Config{
			ServiceMTLS: config.ServiceMTLSConfig{
				Enabled: true,
				Clients: map[string]config.ServiceMTLSClientConfig{
					"authkit.internal": {Scopes: []string{"credits:read"}},
				},
			},
		},
		privateHandler: gin.New(),
	}

	srv.registerServiceRoutes()

	req := httptest.NewRequest(http.MethodGet, "/v1/users/00000000-0000-0000-0000-000000000001/entitlements", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{
			DNSNames: []string{"authkit.internal"},
		}},
	}
	w := httptest.NewRecorder()
	srv.privateHandler.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
}
