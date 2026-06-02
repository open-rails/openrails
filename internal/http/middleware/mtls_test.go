package middleware

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestServiceMTLSRequiredAllowsConfiguredDNSIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(ServiceMTLSRequired(map[string][]string{
		"authkit.internal": {ServiceScopeCreditsRead},
	}))
	r.Use(RequireServiceScope(ServiceScopeCreditsRead))
	r.GET("/service", func(c *gin.Context) {
		identity, ok := c.Get(ServiceIdentityContextKey)
		require.True(t, ok)
		require.Equal(t, "authkit.internal", identity)
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/service", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{
			DNSNames: []string{"authkit.internal"},
		}},
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
}
