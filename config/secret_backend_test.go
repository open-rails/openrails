package config

import "testing"

func TestSecretStoreBackendDerivation(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil config", nil, SecretBackendDB},
		{"explicit db", &Config{SecretBackend: "db"}, SecretBackendDB},
		{"explicit vault", &Config{SecretBackend: "vault"}, SecretBackendVault},
		{"empty + vault enabled -> vault (back-compat)", &Config{Vault: &VaultConfig{Enabled: true}}, SecretBackendVault},
		{"empty + no vault -> db", &Config{}, SecretBackendDB},
		{"explicit db wins over vault enabled", &Config{SecretBackend: "db", Vault: &VaultConfig{Enabled: true}}, SecretBackendDB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.SecretStoreBackend(); got != tc.want {
				t.Fatalf("SecretStoreBackend() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateSecretBackend(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{"db is fine without vault", &Config{SecretBackend: "db"}, false},
		{"vault backend needs vault.enabled", &Config{SecretBackend: "vault"}, true},
		{"vault backend with vault.enabled", &Config{SecretBackend: "vault", Vault: &VaultConfig{Enabled: true}}, false},
		{"derived-vault (empty + enabled) is fine", &Config{Vault: &VaultConfig{Enabled: true}}, false},
		{"unknown value rejected", &Config{SecretBackend: "consul"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSecretBackend(tc.cfg)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateSecretBackend err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
