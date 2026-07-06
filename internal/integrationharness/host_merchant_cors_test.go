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

// #734: host-resolved merchant API hosts + per-merchant CORS preflight.
//
// A public multi-merchant deployment gets ONE standalone server serving
// several merchants, each with its own canonical API hostname. These tests
// prove the engine mechanism: Host resolves to a merchant, CORS preflight
// answers ONLY that merchant's stored allowed_origins, an unrecognized Host
// grants nothing, and a token minted for one merchant is rejected on another
// merchant's Host — all sharing the SAME openrails.merchants directory lookup
// issuer resolution already uses.

// preflight sends an OPTIONS request to url with the given Host + Origin
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

func TestHostMerchantCORSPreflightHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	b := surface.ProvisionOwnedMerchant("host-cors-b-" + strings.ReplaceAll(uuid.NewString(), "-", ""))

	hostA := "api.host-cors-a-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".test"
	hostB := "api.host-cors-b-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".test"
	originA := "https://storefront-a.example"
	originB := "https://storefront-b.example"

	surface.SetMerchantHostConfig(dbtest.TestMerchantID, hostA, []string{originA})
	surface.SetMerchantHostConfig(b.MerchantID, hostB, []string{originB})

	// A's own origin, on A's host: granted.
	resp := preflightHost(t, surface.BaseURL+"/v1/products", hostA, originA)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, originA, resp.Header.Get("Access-Control-Allow-Origin"))
	require.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))

	// B's origin, presented on A's host: fail closed — no global origin union.
	resp = preflightHost(t, surface.BaseURL+"/v1/products", hostA, originB)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Credentials"))

	// B's own origin, on B's host: granted.
	resp = preflightHost(t, surface.BaseURL+"/v1/products", hostB, originB)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, originB, resp.Header.Get("Access-Control-Allow-Origin"))

	// A's origin, presented on B's host: fail closed too (isolation is symmetric).
	resp = preflightHost(t, surface.BaseURL+"/v1/products", hostB, originA)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))

	// A Host that maps to no merchant at all: fail closed.
	resp = preflightHost(t, surface.BaseURL+"/v1/products", "api.unknown-host.test", originA)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))

	// Health checks (merchant-independent) are unaffected by the new Host
	// resolution/CORS middleware — no merchant resolves, no error.
	status, _ := requestJSON(t, http.MethodGet, surface.BaseURL+"/health/live", "", nil)
	require.Equal(t, http.StatusOK, status)
}

// TestHostMerchantRegisteredAfterBootResolvesHTTP is the #734 multi-node proof:
// a merchant's Host/CORS config set AFTER the server already booted and started
// serving requests resolves on the very NEXT request against this SAME running
// server — there is no boot-time host map to restart or refresh. This is the
// load-bearing behavior for a multi-node deployment: a merchant registered on
// node A must resolve on node B without redeploying/restarting node B.
func TestHostMerchantRegisteredAfterBootResolvesHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	host := "api.host-late-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".test"
	origin := "https://late-storefront.example"

	// Before configuring anything, this host resolves nothing (server already
	// running).
	resp := preflightHost(t, surface.BaseURL+"/v1/products", host, origin)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))

	// Provision a brand-new merchant and configure its host/origins — entirely
	// AFTER the server started serving traffic above.
	late := surface.ProvisionOwnedMerchant("host-late-" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	surface.SetMerchantHostConfig(late.MerchantID, host, []string{origin})

	// The SAME already-running server now resolves it, no restart.
	resp = preflightHost(t, surface.BaseURL+"/v1/products", host, origin)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, origin, resp.Header.Get("Access-Control-Allow-Origin"))
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
	surface.SetMerchantHostConfig(dbtest.TestMerchantID, hostA, nil)
	surface.SetMerchantHostConfig(b.MerchantID, hostB, nil)

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
