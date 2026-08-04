//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	return &config.Config{Env: "development", MerchantSource: config.MerchantSourceAPI, TestMode: config.CredentialPostureSandbox}
}

func nmiManifestWithSecurityKey(securityKey string) *BillingConfig {
	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.PSPs = map[string]PSPConfig{
		"mobius": {
			"nmi": {
				AccountID: "681902",
				Secrets: map[string]string{
					"security_key": securityKey,
				},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt
	return manifest
}

// TestReconcileMerchantManifestRefusesLiveNMIUnderTestMode reinstates #348 at
// the manifest arm boundary (#788 deleted the boot-time probe with no
// replacement): a test_mode deployment must refuse to arm an NMI account
// whose credentials belong to a LIVE gateway. The account row must never be
// persisted — the refusal happens before anything is written.
func TestReconcileMerchantManifestRefusesLiveNMIUnderTestMode(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	server := nmiProbeArmTestServer(t, "2") // declined -> LIVE account
	defer server.Close()

	manifest := nmiManifestWithSecurityKey("live-security-key")
	err := ReconcileMerchantManifestData(ctx, testModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{
		Insert:            true,
		NMIProbeV5BaseURL: server.URL,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "PRODUCTION NMI credentials detected while test_mode is enabled")

	var count int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.psps WHERE rail = 'nmi' AND account_id = '681902'
	`).Scan(&count))
	require.Zero(t, count, "a refused arm must never persist the provider account row")
}

// TestReconcileMerchantManifestArmsSimulatedNMIUnderTestMode is the control:
// a genuinely sandboxed NMI account (approves the probe) arms normally under
// test_mode.
func TestReconcileMerchantManifestArmsSimulatedNMIUnderTestMode(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	server := nmiProbeArmTestServer(t, "1") // approved -> simulating
	defer server.Close()

	manifest := nmiManifestWithSecurityKey("sandbox-security-key")
	err := ReconcileMerchantManifestData(ctx, testModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{
		Insert:            true,
		NMIProbeV5BaseURL: server.URL,
	})
	require.NoError(t, err)

	var count int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.psps WHERE rail = 'nmi' AND account_id = '681902'
	`).Scan(&count))
	require.Equal(t, 1, count, "a simulated (sandbox) account arms normally")
}

// TestReconcileMerchantManifestNMIProbeIndeterminateNeverRefuses preserves
// #348's original fail-open posture exactly: a probe error (bad credentials,
// gateway-level rejection) is indeterminate and only warns — it must NEVER
// refuse the boot/arm, because network or credential noise is not evidence
// of a live account.
func TestReconcileMerchantManifestNMIProbeIndeterminateNeverRefuses(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	server := nmiProbeArmTestServer(t, "3") // gateway-level error -> indeterminate
	defer server.Close()

	manifest := nmiManifestWithSecurityKey("unclear-security-key")
	err := ReconcileMerchantManifestData(ctx, testModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{
		Insert:            true,
		NMIProbeV5BaseURL: server.URL,
	})
	require.NoError(t, err, "an indeterminate probe must warn, not refuse (#348)")

	var count int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.psps WHERE rail = 'nmi' AND account_id = '681902'
	`).Scan(&count))
	require.Equal(t, 1, count)
}

// TestReconcileMerchantManifestNMIProbeCooldownRespected proves the #348
// cooldown cache is actually consulted: after a first refusal caches the
// 'live' verdict, a second reconcile pass — even with the fake gateway
// closed (so any accidental re-probe would be a network failure, which only
// warns rather than refusing) — must STILL refuse, proving the refusal came
// from cache rather than a fresh probe.
func TestReconcileMerchantManifestNMIProbeCooldownRespected(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	server := nmiProbeArmTestServer(t, "2") // declined -> LIVE account
	manifest := nmiManifestWithSecurityKey("live-security-key-cooldown")

	err := ReconcileMerchantManifestData(ctx, testModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{
		Insert:            true,
		NMIProbeV5BaseURL: server.URL,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "PRODUCTION NMI credentials")

	server.Close() // any re-probe now fails as a transport error (indeterminate, would only warn)

	err = ReconcileMerchantManifestData(ctx, testModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{
		Insert:            true,
		NMIProbeV5BaseURL: server.URL,
	})
	require.Error(t, err, "a fresh live verdict within the cooldown must refuse from cache, without re-probing the (now-unreachable) gateway")
	require.Contains(t, err.Error(), "cached probe verdict")
}

// TestReconcileMerchantManifestNMIProbeSkippedOutsideTestMode proves
// production deployments never probe-charge on arm: with test_mode=false,
// even an account that would fail the probe (declined test card) arms
// without any network call to the gateway at all.
func TestReconcileMerchantManifestNMIProbeSkippedOutsideTestMode(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("production (test_mode=false) must never probe an NMI account on arm, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.PSPs = map[string]PSPConfig{
		"mobius": {
			"nmi": {
				AccountID: "681902",
				Secrets:   map[string]string{"security_key": "live-security-key-prod"},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	cfg := &config.Config{Env: "development", MerchantSource: config.MerchantSourceAPI, TestMode: config.CredentialPostureLive}
	err := ReconcileMerchantManifestData(ctx, cfg, cp, manifest, MerchantManifestReconcileOptions{
		Insert:            true,
		NMIProbeV5BaseURL: server.URL,
	})
	require.NoError(t, err)

	var count int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.psps WHERE rail = 'nmi' AND account_id = '681902'
	`).Scan(&count))
	require.Equal(t, 1, count)
}
