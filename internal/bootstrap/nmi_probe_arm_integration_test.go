//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

// nmiProbeArmTestServer fakes the NMI v5 gateway's test-mode-probe surface
// (POST /payments/auth + POST /payments/:id/void) — the same shape
// internal/integrations/nmi's own nmi_test.go uses. authCode is the v5
// "response" value: "1" approved (account is simulating), "2" declined (the
// account is LIVE), "3" a gateway-level error (indeterminate).
func nmiProbeArmTestServer(t *testing.T, authCode string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/payments/auth":
			var req struct {
				PaymentDetails struct {
					CardNumber string `json:"card_number"`
				} `json:"payment_details"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.Equal(t, "4111111111111111", req.PaymentDetails.CardNumber)
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-txn","response":"` + authCode + `","response_text":"PROBE","response_code":"100"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/payments/probe-txn/void":
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-txn","response":"1","response_text":"SUCCESS"}`))
		default:
			t.Errorf("unexpected NMI probe request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func testModeReconcileConfig() *config.Config {
	return &config.Config{SecretBackend: config.SecretBackendSnapshot, TestMode: config.CredentialPostureSandbox}
}

func nmiManifestWithSecurityKey(securityKey string) *BillingConfig {
	manifest := hostThreeMerchantManifest()
	mt := manifest.Merchants["host-three"]
	mt.PSPs = map[string]PSPConfig{
		"mobius": {
			"nmi": {
				AccountID: "100002",
				Secrets: map[string]string{
					"security_key": securityKey,
				},
			},
		},
	}
	manifest.Merchants["host-three"] = mt
	return manifest
}

// TestReconcileMerchantManifestDefersSandboxPostureToRuntime: applying a
// manifest is not a posture check. Sandbox NMI credentials persist without any
// provider call; the runtime verifies them once when it loads them and
// disarms (never refuses boot) on anything but a simulated verdict.
func TestReconcileMerchantManifestDefersSandboxPostureToRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()

	require.NoError(t, ReconcileMerchantManifestData(ctx, testModeReconcileConfig(), cp, nmiManifestWithSecurityKey("unverified-security-key"), MerchantManifestReconcileOptions{Insert: true}))
	var count int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM billing.psps WHERE rail = 'nmi' AND account_id = '100002'
	`).Scan(&count))
	require.Equal(t, 1, count)
	require.Zero(t, hits.Load())
}
