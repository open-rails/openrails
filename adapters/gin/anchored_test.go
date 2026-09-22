package openrailsgin

import (
	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/embed"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnchoredBundleRejectsGroupBeforeRegistration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	target := gin.New()
	b := &Bundle{rootOnly: true, routes: []embed.HTTPRoute{{Method: "GET", Path: "/admin/{asset...}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/admin/js/site.js?q=raw", r.RequestURI)
		w.WriteHeader(204)
	})}}}
	require.ErrorContains(t, b.Mount(target.Group("/outer")), "root")
	require.Empty(t, target.Routes())
	require.NoError(t, b.Mount(target))
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		target.ServeHTTP(w, httptest.NewRequest(method, "/admin/js/site.js?q=raw", nil))
		require.Equal(t, 204, w.Code)
	}
}
