package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowserTierRoutesMatchIgnoresMethod(t *testing.T) {
	tier := NewBrowserTierRoutes()
	tier.Add("GET /v1/products")
	tier.Add("POST /v1/checkout")
	tier.Add("GET /v1/me/{id}")

	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/v1/products", true},
		{http.MethodOptions, "/v1/products", true}, // preflight: never itself registered, must still match
		{http.MethodPost, "/v1/checkout", true},
		{http.MethodOptions, "/v1/checkout", true},
		{http.MethodOptions, "/v1/me/abc123", true},
		{http.MethodOptions, "/v1/merchant/settings", false},
		{http.MethodGet, "/v1/nope", false},
	}
	for _, tc := range cases {
		got := tier.Match(httptest.NewRequest(tc.method, tc.path, nil))
		if got != tc.want {
			t.Errorf("Match(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestBrowserTierRoutesNilIsNoMatch(t *testing.T) {
	var tier *BrowserTierRoutes
	if tier.Match(httptest.NewRequest(http.MethodGet, "/v1/products", nil)) {
		t.Fatal("nil *BrowserTierRoutes must match nothing")
	}
}

func permissiveCORSHandler(match func(*http.Request) bool) http.Handler {
	return PermissiveCORSHTTP(match)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// Browser-tier requests get the static wildcard grant, from ANY origin, never
// credentials (#765: bearer JWTs are the security boundary, not an origin
// allow-list).
func TestPermissiveCORSHTTPGrantsWildcardOnMatch(t *testing.T) {
	h := permissiveCORSHandler(AllRequests)

	req := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
	req.Header.Set("Origin", "https://anything.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want empty — never granted", got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (non-preflight request reaches the handler)", rec.Code)
	}
}

// OPTIONS preflight against a browser-tier route is answered directly (204),
// never reaching the handler, and never with credentials.
func TestPermissiveCORSHTTPAnswersPreflightDirectly(t *testing.T) {
	reached := false
	h := PermissiveCORSHTTP(AllRequests)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	req := httptest.NewRequest(http.MethodOptions, "/v1/checkout", nil)
	req.Header.Set("Origin", "https://storefront.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if reached {
		t.Fatal("preflight must not reach the wrapped handler")
	}
}

// Requests that don't match the browser tier get NO CORS headers at all —
// the free, correct posture for admin/platform/merchant-API/webhook/auth
// surfaces, which no browser page's fetch/XHR should ever be able to read
// cross-origin.
func TestPermissiveCORSHTTPGrantsNothingOutsideTier(t *testing.T) {
	tier := NewBrowserTierRoutes()
	tier.Add("GET /v1/products")
	h := permissiveCORSHandler(tier.Match)

	req := httptest.NewRequest(http.MethodGet, "/v1/merchant/settings", nil)
	req.Header.Set("Origin", "https://storefront.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty outside the browser tier", got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — non-tier requests still reach the handler normally", rec.Code)
	}

	// An OPTIONS preflight outside the tier gets no CORS grant either; it
	// simply falls through to whatever the underlying handler does with an
	// unexpected OPTIONS (here, the same 200 stub — real routes 404/405).
	preflight := httptest.NewRequest(http.MethodOptions, "/v1/merchant/settings", nil)
	preflight.Header.Set("Origin", "https://storefront.example")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, preflight)
	if got := rec2.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty for out-of-tier preflight", got)
	}
}

// A nil matcher (equivalent to an empty/unset registry) grants nothing —
// fail closed by default.
func TestPermissiveCORSHTTPNilMatcherGrantsNothing(t *testing.T) {
	h := permissiveCORSHandler(nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
	req.Header.Set("Origin", "https://anything.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}
