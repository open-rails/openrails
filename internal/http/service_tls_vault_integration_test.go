//go:build integration

package server

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/stretchr/testify/require"
)

func TestVaultIssuedServiceMTLSHandshakeAndScope(t *testing.T) {
	certDir := os.Getenv("OPENRAILS_MTLS_TEST_DIR")
	if certDir == "" {
		t.Skip("set OPENRAILS_MTLS_TEST_DIR to a Vault-rendered openrails_mtls volume copy")
	}

	tlsConfig, err := NewServiceTLSConfig(config.ServiceMTLSConfig{
		CertFile:     filepath.Join(certDir, "server.crt"),
		KeyFile:      filepath.Join(certDir, "server.key"),
		ClientCAFile: filepath.Join(certDir, "ca.crt"),
	})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.ServiceMTLSRequired(map[string][]string{
		"doujins.internal": {middleware.ServiceScopeEntitlementsRead},
	}))
	router.GET(
		"/entitlements",
		middleware.RequireServiceScope(middleware.ServiceScopeEntitlementsRead),
		func(c *gin.Context) {
			identity, ok := c.Get(middleware.ServiceIdentityContextKey)
			require.True(t, ok)
			require.Equal(t, "doujins.internal", identity)
			c.Status(http.StatusNoContent)
		},
	)
	router.GET(
		"/credits",
		middleware.RequireServiceScope(middleware.ServiceScopeCreditsRead),
		func(c *gin.Context) {
			c.Status(http.StatusNoContent)
		},
	)

	ts := httptest.NewUnstartedServer(router)
	ts.TLS = tlsConfig
	ts.StartTLS()
	t.Cleanup(ts.Close)

	client := vaultMTLSHTTPClient(t, certDir, "doujins.internal")

	resp, err := client.Get(ts.URL + "/entitlements")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, err = client.Get(ts.URL + "/credits")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func vaultMTLSHTTPClient(t *testing.T, certDir, identity string) *http.Client {
	t.Helper()

	caPEM, err := os.ReadFile(filepath.Join(certDir, "ca.crt"))
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(caPEM))

	cert, err := tls.LoadX509KeyPair(
		filepath.Join(certDir, "clients", identity, "client.crt"),
		filepath.Join(certDir, "clients", identity, "client.key"),
	)
	require.NoError(t, err)

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				RootCAs:      roots,
				ServerName:   "localhost",
				Certificates: []tls.Certificate{cert},
			},
		},
		Timeout: 5 * time.Second,
	}
}
