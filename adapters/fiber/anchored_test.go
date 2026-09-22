package openrailsfiber

import (
	"github.com/gofiber/fiber/v3"
	"github.com/open-rails/openrails/embed"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnchoredBundleRejectsGroupBeforeRegistration(t *testing.T) {
	target := fiber.New()
	b := &Bundle{rootOnly: true, routes: []embed.HTTPRoute{{Method: "GET", Path: "/admin/{asset...}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/admin/js/site.js?q=raw", r.RequestURI)
		w.WriteHeader(204)
	})}}}
	require.ErrorContains(t, b.Mount(target.Group("/outer")), "root")
	require.Empty(t, target.GetRoutes())
	require.NoError(t, b.Mount(target))
	for _, method := range []string{"GET", "HEAD"} {
		response, err := target.Test(httptest.NewRequest(method, "/admin/js/site.js?q=raw", nil))
		require.NoError(t, err)
		require.Equal(t, 204, response.StatusCode)
		response.Body.Close()
	}
}
