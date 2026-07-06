//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #734: host-resolved merchant API hosts (browser CORS was cut in #765 — see
// browser_tier_cors_test.go for the static per-route-tier policy that
// replaced it).
//
// A public multi-merchant deployment gets ONE standalone server serving
// several merchants, each with its own canonical API hostname. These tests
// prove the engine mechanism: Host resolves to a merchant live (no
// boot-time map, no restart needed), an unrecognized Host resolves nothing,
// and a token minted for one merchant is rejected on another merchant's Host
// — all sharing the SAME openrails.merchants directory lookup issuer
// resolution already uses.

// preflightHost sends an OPTIONS request to url with the given Host + Origin
// headers, mirroring a browser CORS preflight.
func preflightHost(t *testing.T, url, host, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, url, nil)
	require.NoError(t, err)
	req.Host = host
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// requestJSONHost is requestJSON plus an explicit Host header override — a
// browser-style request against a specific merchant's api_host rather than
// the httptest server's own bare address.
func requestJSONHost(t *testing.T, method, url, host, token string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequest(method, url, &buf)
	require.NoError(t, err)
	if host != "" {
		req.Host = host
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

// TestHostMerchantRegisteredAfterBootResolvesHTTP is the #734 multi-node
// proof: a merchant's Host config set AFTER the server already booted and
// started serving requests resolves on the very NEXT request against this
// SAME running server — there is no boot-time host map to restart or
// refresh. Observable via the Host-vs-issuer mismatch check (unrelated to
// CORS since #765): a token valid for merchant B is accepted against a host
// that resolves to no merchant at all (Host resolution is a no-op there),
// then rejected the instant that SAME host is mapped, live, to a DIFFERENT
// merchant — proving the new mapping took effect immediately, no restart.
func TestHostMerchantRegisteredAfterBootResolvesHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	b := surface.ProvisionOwnedMerchant("host-late-b-" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	tokenB := surface.RegisterServiceJWTIssuer(
		"host-late-issuer-b-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		b.MerchantSlug,
		[]string{controlplane.PermMerchantCustomerSettingsRead, controlplane.PermMerchantCustomerSettingsUpdate},
	)

	host := "api.host-late-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".test"
	payer := uuid.NewString()

	// Before any Host config exists for `host`, it resolves no merchant at
	// all: Host resolution is a no-op there, so B's own token works fine
	// against it (falls back to ordinary issuer-based resolution).
	status, body := requestJSONHost(t, http.MethodGet,
		surface.BaseURL+"/v1/merchant/credits/balance?currency=usd&customer_id="+payer, host, tokenB.Token, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	// Provision a brand-new "late" merchant and map THIS SAME host to it —
	// entirely AFTER the server started serving traffic above.
	late := surface.ProvisionOwnedMerchant("host-late-late-" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	surface.SetMerchantHostConfig(late.MerchantID, host)

	// The SAME already-running server now resolves `host` to `late`, no
	// restart: B's token is rejected on it, since Host-merchant (late) now
	// disagrees with the token's own issuer-merchant (B).
	status, body = requestJSONHost(t, http.MethodGet,
		surface.BaseURL+"/v1/merchant/credits/balance?currency=usd&customer_id="+payer, host, tokenB.Token, nil)
	require.Containsf(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status,
		"host must resolve the newly-configured merchant live, no restart: %s", string(body))
}

// TestHostMerchantVsIssuerMismatchHTTP proves the request-path fail-closed
// check (#734): a service JWT minted for merchant A's issuer is rejected when
// presented against merchant B's Host, even though the token itself verifies
// perfectly (signature/issuer/audience all valid for A) — Host-merchant must
// equal issuer-merchant on merchant-scoped routes.
func TestHostMerchantVsIssuerMismatchHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	b := surface.ProvisionOwnedMerchant("host-mismatch-b-" + strings.ReplaceAll(uuid.NewString(), "-", ""))

	hostA := "api.host-mismatch-a-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".test"
	hostB := "api.host-mismatch-b-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".test"
	surface.SetMerchantHostConfig(dbtest.TestMerchantID, hostA)
	surface.SetMerchantHostConfig(b.MerchantID, hostB)

	tokenA := surface.RegisterServiceJWTIssuer(
		"host-mismatch-issuer-a-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		dbtest.TestMerchantSlug,
		[]string{controlplane.PermMerchantCustomerSettingsRead, controlplane.PermMerchantCustomerSettingsUpdate},
	)

	payer := uuid.NewString()

	// A's own token, on A's own host: works normally.
	status, body := requestJSONHost(t, http.MethodPost, surface.BaseURL+"/v1/merchant/credits/deposit", hostA, tokenA.Token, map[string]any{
		"customer_id": payer,
		"invoker":     "host-mismatch-invoker",
		"currency":    "usd",
		"amount":      1000,
		"source":      "host-mismatch-test",
		"source_id":   uuid.NewString(),
	})
	require.Equal(t, http.StatusOK, status, string(body))

	// A's own token, presented on B's host: fails closed. The token is a
	// perfectly valid A credential — Host disagreement alone must reject it.
	status, body = requestJSONHost(t, http.MethodGet,
		surface.BaseURL+"/v1/merchant/credits/balance?currency=usd&customer_id="+payer, hostB, tokenA.Token, nil)
	require.Containsf(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status,
		"A's token on B's host must fail closed: %s", string(body))

	// Sanity: the SAME token on ITS OWN host still works (the mismatch check
	// isn't just rejecting everything).
	status, body = requestJSONHost(t, http.MethodGet,
		surface.BaseURL+"/v1/merchant/credits/balance?currency=usd&customer_id="+payer, hostA, tokenA.Token, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	// No Host override at all (the server's own httptest host, unconfigured):
	// unaffected — single-host deployments see no behavior change.
	status, body = requestJSON(t, http.MethodGet,
		surface.BaseURL+"/v1/merchant/credits/balance?currency=usd&customer_id="+payer, tokenA.Token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
}
