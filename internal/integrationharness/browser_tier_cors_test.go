//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// #765: per-merchant CORS is gone (bearer JWTs make an origin allowlist pure
// overhead — a stolen token replays fine from curl, where CORS doesn't
// exist). What replaced it is a STATIC policy scoped to route TIER, not to
// merchant/Host: the browser-facing checkout + self-service surfaces answer
// `Access-Control-Allow-Origin: *` (never credentials) from ANY origin, with
// zero api_host/allowed_origins configuration required; every other surface
// (admin console, platform, merchant API, webhooks, control-plane auth)
// emits no CORS headers at all. These tests hit the real standalone server —
// no per-merchant host config is set anywhere, proving the grant is
// unconditional on the browser tier and absent everywhere else.

// TestBrowserTierCORSGrantsWildcardFromAnyOrigin proves the static policy on
// the checkout (buyer-facing) and self-service (delegated customer) route
// sets: any Origin, on the plain unconfigured server host (no api_host, no
// merchant Host mapping — #765 needs none), gets the wildcard grant, never
// credentials, both at preflight and on the real response.
func TestBrowserTierCORSGrantsWildcardFromAnyOrigin(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	for _, tc := range []struct {
		name string
		path string
	}{
		{"checkout", "/v1/products"},
		{"self-service", "/v1/me/balance"},
	} {
		for _, origin := range []string{"https://storefront-a.example", "https://totally-unconfigured.example"} {
			resp := preflightHost(t, surface.BaseURL+tc.path, "", origin)
			require.Equal(t, http.StatusNoContent, resp.StatusCode, "%s preflight from %s", tc.name, origin)
			require.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"), "%s preflight from %s", tc.name, origin)
			require.Empty(t, resp.Header.Get("Access-Control-Allow-Credentials"), "%s preflight from %s: credentials must never be granted", tc.name, origin)
		}

		// The actual (non-preflight) response also carries the grant — the
		// status code isn't the point here (auth/params may 4xx), the header is.
		req, err := http.NewRequest(http.MethodGet, surface.BaseURL+tc.path, nil)
		require.NoError(t, err)
		req.Header.Set("Origin", "https://storefront-a.example")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"), "%s real response", tc.name)
		require.Empty(t, resp.Header.Get("Access-Control-Allow-Credentials"), "%s real response", tc.name)
		_ = resp.Body.Close()
	}
}

// TestBrowserTierCORSGrantsNothingOutsideBrowserTier proves the negative: the
// admin console, cross-merchant platform directory, merchant/service API,
// inbound webhooks, and control-plane auth surfaces get NO CORS headers at
// all, from any origin, at preflight or on the real response — a browser
// refuses cross-origin script access to them by default, which is the
// correct, free posture (only bearer-JWT curl/service callers use these,
// never a browser page's fetch/XHR).
func TestBrowserTierCORSGrantsNothingOutsideBrowserTier(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	paths := []string{
		"/admin/",
		"/v1/platform/merchants",
		"/v1/merchant/credits/balance",
		"/v1/webhooks/stripe",
		"/auth/login",
	}
	for _, path := range paths {
		resp := preflightHost(t, surface.BaseURL+path, "", "https://storefront-a.example")
		require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"), "preflight %s must grant no browser CORS", path)
		require.Empty(t, resp.Header.Get("Access-Control-Allow-Credentials"), "preflight %s", path)

		req, err := http.NewRequest(http.MethodGet, surface.BaseURL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Origin", "https://storefront-a.example")
		real, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Empty(t, real.Header.Get("Access-Control-Allow-Origin"), "real response %s must grant no browser CORS", path)
		_ = real.Body.Close()
	}
}
