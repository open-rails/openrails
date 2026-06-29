package embedhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCapabilitiesHandler(t *testing.T) {
	// Advertise checkout+customer+webhooks; everything else must read false.
	h := CapabilitiesHandler([]RouteSet{RouteSetCheckout, RouteSetCustomer, RouteSetWebhooks})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/capabilities", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("missing ETag")
	}

	var caps Capabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Every known group must be present (the ranged-map contract), so a client can
	// ask about any group without knowing the closed-world set.
	if len(caps.RouteGroups) != len(AllRouteSets) {
		t.Fatalf("route_groups has %d keys, want %d (all known groups)", len(caps.RouteGroups), len(AllRouteSets))
	}
	want := map[RouteSet]bool{
		RouteSetCheckout:         true,
		RouteSetCustomer:         true, // advertised even though the user handler strips it
		RouteSetWebhooks:         true,
		RouteSetMerchantAdmin:    false,
		RouteSetCatalog:          false,
		RouteSetPaymentProviders: false,
		RouteSetMerchantAPI:      false,
	}
	for rs, w := range want {
		if caps.RouteGroups[rs] != w {
			t.Errorf("route_groups[%s] = %v, want %v", rs, caps.RouteGroups[rs], w)
		}
	}
}

func TestCapabilitiesHandlerETagNotModified(t *testing.T) {
	h := CapabilitiesHandler([]RouteSet{RouteSetCheckout})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/capabilities", nil))
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}

	req := httptest.NewRequest(http.MethodGet, "/billing/v1/capabilities", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional GET status = %d, want 304", rec2.Code)
	}
}

func TestResolveRouteSets(t *testing.T) {
	// nil → embedded defaults.
	if got := ResolveRouteSets(nil); len(got) != len(EmbeddedDefaultRouteSets) {
		t.Errorf("ResolveRouteSets(nil) len = %d, want %d", len(got), len(EmbeddedDefaultRouteSets))
	}
	// Dedup + drop empties, preserve order.
	got := ResolveRouteSets([]RouteSet{RouteSetCheckout, "", RouteSetCheckout, RouteSetWebhooks})
	want := []RouteSet{RouteSetCheckout, RouteSetWebhooks}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
