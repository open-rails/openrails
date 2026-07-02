package embedded

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCombinedMountRoutesAndRewrites(t *testing.T) {
	record := func(name string, got *string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*got = name + " " + r.URL.Path
			w.WriteHeader(http.StatusOK)
		})
	}
	var got string
	mount := combinedMount("/api/openrails", "/billing", record("self", &got), record("user", &got))

	cases := []struct{ path, want string }{
		// /me + /customers subtrees -> self handler, rewritten to canonical /billing/v1.
		{"/api/openrails/v1/me/status", "self /billing/v1/me/status"},
		{"/api/openrails/v1/me/subscriptions", "self /billing/v1/me/subscriptions"},
		{"/api/openrails/v1/customers/acme/spend-delegations", "self /billing/v1/customers/acme/spend-delegations"},
		// Everything else -> user handler.
		{"/api/openrails/v1/merchant/subscriptions", "user /billing/v1/merchant/subscriptions"},
		{"/api/openrails/v1/merchant", "user /billing/v1/merchant"},
		{"/api/openrails/v1/products", "user /billing/v1/products"},
		{"/api/openrails/v1/webhooks/mobius", "user /billing/v1/webhooks/mobius"},
		// /v1/meow merely starts like /v1/me — NOT the self subtree.
		{"/api/openrails/v1/meow", "user /billing/v1/meow"},
	}
	for _, tc := range cases {
		got = ""
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		mount.ServeHTTP(httptest.NewRecorder(), req)
		if got != tc.want {
			t.Errorf("%s: dispatched %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestRouteSetSelectedCustomer(t *testing.T) {
	if !routeSetSelected(nil, RouteSetCustomer) {
		t.Fatal("default embedded route sets should include customer routes")
	}
	if routeSetSelected([]RouteSet{RouteSetCheckout}, RouteSetCustomer) {
		t.Fatal("explicit route sets without customer should not include customer routes")
	}
}

func TestCombinedMountSkipsCustomerHandlerWhenNotSelected(t *testing.T) {
	self := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("self handler should not run when customer routes are omitted")
	})
	user := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	handler := combinedMount("/api/openrails", "/billing", nil, user)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/openrails/v1/me/balance", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}

	handler = combinedMount("/api/openrails", "/billing", self, user)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/openrails/v1/merchant/catalog/products", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("merchant status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}
