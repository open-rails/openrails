package merchantsecrets

import (
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/vault"
)

// gateSecretBackend is the #661 decision: where merchant secrets are served from,
// given the DECLARED backend and the probed Vault capabilities. The capability
// probe itself is integration-tested (capabilities_integration_test.go); this pins
// the decision matrix deterministically.
func TestGateSecretBackend(t *testing.T) {
	full := vault.Capabilities{KVRead: true, KVWrite: true}
	transitOnly := vault.Capabilities{} // transit-only policy: no KV caps
	kvReadOnly := vault.Capabilities{KVRead: true}

	cases := []struct {
		name           string
		backend        string
		vaultConnected bool
		caps           vault.Capabilities
		wantUseVault   bool
		wantErr        bool
	}{
		{"db backend ignores vault", config.SecretBackendDB, true, full, false, false},
		{"db backend, transit-only vault", config.SecretBackendDB, true, transitOnly, false, false},
		{"db backend, no vault", config.SecretBackendDB, false, vault.Capabilities{}, false, false},
		{"vault backend + full KV", config.SecretBackendVault, true, full, true, false},
		{"vault backend + read-only KV still serves", config.SecretBackendVault, true, kvReadOnly, true, false},
		{"vault backend + transit-only = boot error (no KV read)", config.SecretBackendVault, true, transitOnly, false, true},
		{"vault backend + not connected = error", config.SecretBackendVault, false, vault.Capabilities{}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useVault, err := gateSecretBackend(tc.backend, tc.vaultConnected, tc.caps)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got useVault=%v nil err", useVault)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if useVault != tc.wantUseVault {
				t.Fatalf("useVault=%v, want %v", useVault, tc.wantUseVault)
			}
		})
	}
}

// enforceEncryptionPosture is the #667 boot gate: DB-backed store + disabled
// encryptor refuses boot outside development (mirrors db.EnforceRLSPosture).
func TestEnforceEncryptionPosture(t *testing.T) {
	cases := []struct {
		name             string
		enabled, require bool
		wantErr          bool
	}{
		{"prod + no key = boot refused", false, true, true},
		{"dev + no key = allowed (warns)", false, false, false},
		{"prod + key = allowed", true, true, false},
		{"dev + key = allowed", true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := enforceEncryptionPosture(tc.enabled, tc.require)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want boot-refusal error, got nil")
				}
				// The error must be actionable: name the key and the Vault alternative.
				for _, want := range []string{"ENCRYPTION_MASTER_KEY", "secret_backend=vault"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q missing %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// RequiresSecretEncryption pins the env signal to the same gate as RequiresRLS.
func TestRequiresSecretEncryption_EnvSignal(t *testing.T) {
	cases := []struct {
		env  string
		want bool
	}{
		{"", false}, {"dev", false}, {"development", false},
		{"production", true}, {"staging", true},
	}
	for _, tc := range cases {
		cfg := &config.Config{Env: tc.env}
		if got := cfg.RequiresSecretEncryption(); got != tc.want {
			t.Fatalf("Env=%q: RequiresSecretEncryption()=%v, want %v", tc.env, got, tc.want)
		}
		if got, rls := cfg.RequiresSecretEncryption(), cfg.RequiresRLS(); got != rls {
			t.Fatalf("Env=%q: encryption gate %v diverges from RLS gate %v", tc.env, got, rls)
		}
	}
	var nilCfg *config.Config
	if nilCfg.RequiresSecretEncryption() {
		t.Fatal("nil config must not require encryption (mirrors RequiresRLS)")
	}
}

// deriveRouteGates decides the advisory SolanaCanSign/SecretWrite route gates (#661).
func TestDeriveRouteGates(t *testing.T) {
	cases := []struct {
		name                               string
		useVault, connected, encryption    bool
		caps                               vault.Capabilities
		wantSolanaCanSign, wantSecretWrite bool
	}{
		{"db + encryption, no vault: local keys sign, writes on", false, false, true, vault.Capabilities{}, true, true},
		{"db, no encryption, no vault: cannot sign", false, false, false, vault.Capabilities{}, false, true},
		{"db + transit-only vault: connection signs, writes on", false, true, false, vault.Capabilities{}, true, true},
		{"vault backend + KV rw: signs via KV keys, writes on", true, true, false, vault.Capabilities{KVRead: true, KVWrite: true}, true, true},
		{"vault backend + KV read-only: signs, writes OFF", true, true, false, vault.Capabilities{KVRead: true}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canSign, secretWrite := deriveRouteGates(tc.useVault, tc.connected, tc.encryption, tc.caps)
			if canSign != tc.wantSolanaCanSign || secretWrite != tc.wantSecretWrite {
				t.Fatalf("got (canSign=%v, secretWrite=%v), want (%v, %v)", canSign, secretWrite, tc.wantSolanaCanSign, tc.wantSecretWrite)
			}
		})
	}
}
