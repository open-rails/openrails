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
		{"/api/openrails/v1/webhooks/nmi", "user /billing/v1/webhooks/nmi"},
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

// TestCombinedMountPrefixNormalizationAndBoundary covers #769: a trailing
// slash on mountPrefix must not corrupt every rewrite, prefix matching must
// respect a "/" boundary (not just a byte prefix), and paths outside the
// mount must 404 cleanly instead of getting a garbage rewrite.
func TestCombinedMountPrefixNormalizationAndBoundary(t *testing.T) {
	ok := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}
	user := http.HandlerFunc(ok)

	cases := []struct {
		name        string
		mountPrefix string
		path        string
		wantStatus  int
		wantPath    string // only checked when wantStatus == 200
	}{
		{
			name:        "trailing slash prefix serves identically to canonical",
			mountPrefix: "/billing/",
			path:        "/billing/v1/products",
			wantStatus:  http.StatusOK,
			wantPath:    "/base/v1/products",
		},
		{
			name:        "double trailing slash prefix also normalizes",
			mountPrefix: "/billing//",
			path:        "/billing/v1/products",
			wantStatus:  http.StatusOK,
			wantPath:    "/base/v1/products",
		},
		{
			name:        "canonical no-trailing-slash prefix unaffected",
			mountPrefix: "/billing",
			path:        "/billing/v1/products",
			wantStatus:  http.StatusOK,
			wantPath:    "/base/v1/products",
		},
		{
			name:        "non-matching path 404s cleanly",
			mountPrefix: "/billing",
			path:        "/other/v1/products",
			wantStatus:  http.StatusNotFound,
		},
		{
			name:        "prefix-boundary near-miss 404s",
			mountPrefix: "/billing",
			path:        "/billingfoo/v1/products",
			wantStatus:  http.StatusNotFound,
		},
		{
			name:        "empty prefix unchanged",
			mountPrefix: "",
			path:        "/v1/products",
			wantStatus:  http.StatusOK,
			wantPath:    "/base/v1/products",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mountPrefix, err := normalizeMountPrefix(tc.mountPrefix)
			if err != nil {
				t.Fatalf("normalizeMountPrefix(%q): %v", tc.mountPrefix, err)
			}
			mount := combinedMount(mountPrefix, "/base", nil, user)
			rec := httptest.NewRecorder()
			mount.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK && rec.Header().Get("X-Path") != tc.wantPath {
				t.Fatalf("rewritten path = %q, want %q", rec.Header().Get("X-Path"), tc.wantPath)
			}
		})
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
