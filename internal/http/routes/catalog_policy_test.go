package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/stretchr/testify/require"
)

// Record real mux registrations: router 404/405 alone does not establish that
// a mutation was omitted instead of mounted with rejecting middleware.
func TestCatalogMutationRouteInventory(t *testing.T) {
	for _, source := range []string{config.MerchantConfigSourceManifest, config.MerchantConfigSourceAPI} {
		for _, allow := range []bool{false, true} {
			rt := &app.Runtime{Config: &config.Config{MerchantConfigSource: source, AllowCatalogUpdates: allow}}
			var inventory []string
			mux := http.NewServeMux()
			record := func(pattern string) { inventory = append(inventory, pattern) }
			RegisterCatalogRoutes(router.NewMuxRecorded(mux, "/merchant/catalog", rt, record), rt, Options{})
			RegisterOwnedCatalogRoutes(router.NewMuxRecorded(mux, "/catalog", rt, record), rt, Options{})
			RegisterCatalogCollectionRoutes(router.NewMuxRecorded(mux, "/merchant/catalogs", rt, record), rt, Options{})
			for _, route := range []string{"GET /merchant/catalog/revision", "GET /merchant/catalog/products", "GET /merchant/catalog/prices", "GET /merchant/catalog/meters", "GET /catalog", "GET /catalog/products", "GET /merchant/catalogs"} {
				require.Contains(t, inventory, route)
			}
			mutations := 0
			for _, route := range inventory {
				if !strings.HasPrefix(route, "GET ") && !strings.HasPrefix(route, "HEAD ") && !strings.HasPrefix(route, "OPTIONS ") {
					mutations++
					require.True(t, allow, "disabled catalog mutation registered: %s", route)
				}
			}
			if allow {
				require.Positive(t, mutations)
				for _, route := range []string{"POST /merchant/catalog/applications", "POST /merchant/catalog/products", "PUT /merchant/catalog/products/by-key/{key}", "POST /merchant/catalog/prices", "POST /merchant/catalog/prices/{id}/key", "PUT /merchant/catalog/meters/{key}", "DELETE /merchant/catalog/meters/{key}/rate-card", "PUT /catalog", "POST /catalog/products", "POST /catalog/prices", "POST /merchant/catalogs"} {
					require.Contains(t, inventory, route)
				}
			}
		}
	}
}

func TestCatalogWriteGuardRejectsEvenWhenMounted(t *testing.T) {
	called := false
	handler := catalogWriteGuardMW(&config.Config{})(func(*httprequest.Request) { called = true })
	recorder := httptest.NewRecorder()
	handler(httprequest.NewHTTP(recorder, httptest.NewRequest(http.MethodPost, "/products", nil), nil))
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), "catalog_updates_disabled")
	require.False(t, called)
}

func TestNegotiatedRateMutationRouteInventory(t *testing.T) {
	for _, allow := range []bool{false, true} {
		rt := &app.Runtime{Config: &config.Config{AllowCatalogUpdates: allow}}
		var inventory []string
		mux := http.NewServeMux()
		record := func(pattern string) { inventory = append(inventory, pattern) }
		RegisterMerchantActionRoutes(router.NewMuxRecorded(mux, "/merchant", rt, record), rt, Options{})
		require.Contains(t, inventory, "GET /merchant/customers/{customer_id}/rate-overrides")
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			route := method + " /merchant/customers/{customer_id}/rate-overrides/{meter_key}"
			if allow {
				require.Contains(t, inventory, route)
			} else {
				require.NotContains(t, inventory, route)
			}
		}
		require.Contains(t, inventory, "POST /merchant/customers/{customer_id}/credits", "credit grants are independent of catalog authoring")
	}
}
