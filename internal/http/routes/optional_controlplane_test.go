package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/http/router"
)

func TestAbsentControlPlaneDoesNotMountManagementRoutes(t *testing.T) {
	mux := http.NewServeMux()
	RegisterMerchantActionRoutes(router.NewMux(mux, "/v1/merchant", nil), nil, Options{})
	for _, request := range []struct{ method, path string }{
		{"GET", "/api-keys"}, {"POST", "/api-keys"}, {"DELETE", "/api-keys/key"},
		{"GET", "/team"}, {"GET", "/team/invites"}, {"POST", "/team/invites"},
		{"DELETE", "/team/invites/id"}, {"PATCH", "/team/id"}, {"DELETE", "/team/id"},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(request.method, "/v1/merchant"+request.path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("unavailable route %s %s returned %d", request.method, request.path, w.Code)
		}
	}
}
