//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// fakeNMIProbeServer fakes the NMI v5 test-mode-probe surface (#348): POST
// /payments/auth answers "1" (approved → the account is simulating, i.e. a
// genuine sandbox) and the follow-up void succeeds. hits counts auth probes.
func fakeNMIProbeServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/payments/auth":
			hits.Add(1)
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-txn","response":"1","response_text":"PROBE","response_code":"100"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/payments/probe-txn/void":
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-txn","response":"1","response_text":"SUCCESS"}`))
		default:
			t.Errorf("unexpected NMI probe request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// testSigningKeyPEM generates an RSA signing key so the booted server and the
// test's token-minting control-plane attach share the SAME key material
// (dev-ephemeral keys are per-attach, so tokens would not cross-verify).
func testSigningKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func writeMode1Config(t *testing.T, dir, dsn string, port int, merchantSource, keyPEM string) string {
	t.Helper()
	cfgYAML := fmt.Sprintf(`env: development
merchant_source: %s
test_mode: sandbox
provider_write_mode: full
host: 127.0.0.1
port: %d
db:
  url: %s
auth:
  issuer: https://controlplane.openrails.test
  active_key_id: mode1-test-key
  active_private_key_pem: |
%s
`, merchantSource, port, dsn, indentPEM(keyPEM))
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfgYAML), 0o600))
	return path
}

func indentPEM(pemStr string) string {
	lines := strings.Split(strings.TrimRight(pemStr, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

const mode1TestMerchantSlug = "chaos-mode1"

func writeMode1Manifest(t *testing.T, dir string) string {
	t.Helper()
	manifest := fmt.Sprintf(`version: 1
merchants:
  %s:
    display_name: Chaos Mode1
    psps:
      mobius:
        nmi:
          account_id: "579145"
          secrets:
            security_key: sandbox-security-key
`, mode1TestMerchantSlug)
	path := filepath.Join(dir, "merchants.yaml")
	require.NoError(t, os.WriteFile(path, []byte(manifest), 0o600))
	return path
}

// TestRunServerMode1BootArmsNMIPSPFromManifest proves #847 end-to-end at the
// REAL server entrypoint: `openrails run-server` in MODE 1
// (merchant_source=manifest) converges the boot merchant manifest — merchant +
// psps rows as DB projections, secrets held ONLY in the in-memory plane — and
// the armed NMI PSP serves checkout-rail readiness over real HTTP. The #348
// test_mode arm probe hits an injected fake gateway, never the live sandbox.
func TestRunServerMode1BootArmsNMIPSPFromManifest(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	var probeHits atomic.Int64
	probe := fakeNMIProbeServer(t, &probeHits)
	defer probe.Close()
	bootNMIProbeV5BaseURL = probe.URL
	defer func() { bootNMIProbeV5BaseURL = "" }()

	dir := t.TempDir()
	manifestPath := writeMode1Manifest(t, dir)
	port := freeTCPPort(t)
	keyPEM := testSigningKeyPEM(t)
	cfgPath := writeMode1Config(t, dir, dsn, port, "manifest", keyPEM)

	root := newRootCmd()
	root.SetArgs([]string{"run-server", "--config", cfgPath, "--merchant-manifest", manifestPath, "--no-workers"})
	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForServerReady(t, base+"/readyz", done)

	// The manifest converged as DB projections: merchant row + armed NMI psps
	// row — and the #348 probe went through the injected fake gateway.
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	var merchantID, permissionGroupID string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT id::text, COALESCE(permission_group_id, '') FROM openrails.merchants WHERE slug = $1
	`, mode1TestMerchantSlug).Scan(&merchantID, &permissionGroupID))
	require.NotEmpty(t, permissionGroupID, "manifest boot must bind the merchant permission group")

	// psps and merchant_secrets are RLS-FORCED and this pool connects as the
	// unprivileged openrails_app role, so an UNSCOPED read returns zero rows with
	// no error. Both assertions below must run inside a merchant scope — the
	// secrets one especially, since "expect zero" is what an unscoped read
	// returns unconditionally, i.e. it would pass while proving nothing.
	scoped, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = scoped.Rollback(ctx) }()
	_, err = scoped.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, merchantID)
	require.NoError(t, err)

	var pspCount int
	require.NoError(t, scoped.QueryRow(ctx, `
		SELECT count(*) FROM openrails.psps
		 WHERE merchant_id = $1 AND rail = 'nmi' AND environment = 'test'
		   AND account_id = '579145' AND NOT archived
	`, merchantID).Scan(&pspCount))
	require.Equal(t, 1, pspCount, "MODE-1 boot must arm the manifest NMI PSP")
	require.Positive(t, probeHits.Load(), "the #348 test_mode arm probe must hit the injected gateway")

	// MODE 1 holds credentials in memory only — nothing lands in the
	// persistent secret store.
	var secretCount int
	require.NoError(t, scoped.QueryRow(ctx, `
		SELECT count(*) FROM openrails.merchant_secrets WHERE merchant_id = $1
	`, merchantID).Scan(&secretCount))
	require.Zero(t, secretCount, "MODE-1 secrets must never persist (#723)")
	require.NoError(t, scoped.Rollback(ctx))

	// Checkout-rail readiness over real HTTP: resolve the merchant per-Host
	// (#734) and prove the armed rail passes checkoutRailConfigured while an
	// unarmed rail is refused. The user token is minted through a second
	// control-plane attach sharing the config-declared signing key.
	_, err = pool.Exec(ctx, `UPDATE openrails.merchants SET api_host = 'chaos-mode1.test' WHERE id = $1`, merchantID)
	require.NoError(t, err)

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	mintApp := &app.App{Config: cfg}
	defer func() { _ = mintApp.Close(context.Background()) }()
	require.NoError(t, embcp.Attach(ctx, mintApp, cfg, nil))
	cp := embcp.Get(mintApp)
	require.NotNil(t, cp)
	username := "mode1user" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	user, err := cp.Core().CreateUser(ctx, username+"@example.com", username)
	require.NoError(t, err)
	token, _, err := cp.Core().MintAccessToken(ctx, user.ID, nil)
	require.NoError(t, err)

	checkout := func(rail string) (int, string) {
		body, err := json.Marshal(map[string]any{
			"price_id": "price_mode1boot",
			"payment":  map[string]any{"rail": rail},
		})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/checkout", bytes.NewReader(body))
		require.NoError(t, err)
		req.Host = "chaos-mode1.test"
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(respBody)
	}

	status, respBody := checkout("stripe")
	require.Equal(t, http.StatusBadRequest, status, "unarmed rail must be refused: %s", respBody)
	require.Contains(t, respBody, "unsupported rail")

	status, respBody = checkout("nmi")
	require.NotContains(t, respBody, "unsupported rail",
		"manifest-armed NMI must pass checkout-rail readiness (status %d)", status)
	require.NotEqual(t, http.StatusUnauthorized, status, "user token must verify: %s", respBody)
	require.NotEqual(t, http.StatusForbidden, status, "user token must authorize: %s", respBody)

	shutdownRunServer(t, done)
}

// TestRunServerRefusesMissingExplicitMerchantManifest: an EXPLICIT
// --merchant-manifest path that does not exist refuses boot (#723 boot matrix).
func TestRunServerRefusesMissingExplicitMerchantManifest(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dir := t.TempDir()
	cfgPath := writeMode1Config(t, dir, dsn, 0, "manifest", testSigningKeyPEM(t))

	root := newRootCmd()
	root.SetArgs([]string{"run-server", "--config", cfgPath, "--merchant-manifest", filepath.Join(dir, "absent.yaml"), "--no-workers"})
	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "merchant manifest")
}

// TestRunServerAPIModeRefusesMerchantManifest: merchant_source=api refuses a
// present boot manifest — two truths (#723).
func TestRunServerAPIModeRefusesMerchantManifest(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dir := t.TempDir()
	manifestPath := writeMode1Manifest(t, dir)
	cfgPath := writeMode1Config(t, dir, dsn, 0, "api", testSigningKeyPEM(t))

	root := newRootCmd()
	root.SetArgs([]string{"run-server", "--config", cfgPath, "--merchant-manifest", manifestPath, "--no-workers"})
	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "merchant_source=api refuses")
}

func waitForServerReady(t *testing.T, readyURL string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("run-server exited before becoming ready: %v", err)
		default:
		}
		resp, err := http.Get(readyURL)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("run-server did not become ready in time")
}

func shutdownRunServer(t *testing.T, done <-chan error) {
	t.Helper()
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	select {
	case err := <-done:
		require.NoError(t, err, "run-server must shut down cleanly")
	case <-time.After(45 * time.Second):
		t.Fatal("run-server did not shut down after SIGTERM")
	}
}
