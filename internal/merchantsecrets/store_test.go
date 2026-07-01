package merchantsecrets

import (
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
