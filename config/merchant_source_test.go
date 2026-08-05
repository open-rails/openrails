package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

// #723 boot matrix: the config-posture rows.

func validAPIBase(env string) *Config {
	return &Config{
		Env:               env,
		MerchantSource:    MerchantSourceAPI,
		ProviderWriteMode: ProviderWriteModeReadOnly,
		DB:                &DBConfig{URL: "postgres://u:p@localhost:5432/x"},
		// #742: Validate refuses nil rate_limits outside development —
		// orthogonal to what this test exercises (merchant_source), so give it
		// a posture rather than tripping that gate incidentally.
		RateLimits: GetDefaultBillingConfig().RateLimits,
	}
}

func TestMerchantSourceDefaultsToManifest(t *testing.T) {
	cfg := &Config{}
	if got := cfg.MerchantSourceMode(); got != MerchantSourceManifest {
		t.Fatalf("empty merchant_source = %q, want manifest", got)
	}
	if !cfg.IsManifestMerchantSource() {
		t.Fatal("empty merchant_source must be MODE 1")
	}
	var nilCfg *Config
	if !nilCfg.IsManifestMerchantSource() {
		t.Fatal("nil config must default to manifest mode")
	}
}

func TestMerchantSourceUnknownValueRefusesLoad(t *testing.T) {
	cfg := &Config{Env: "development", MerchantSource: "yaml", DB: &DBConfig{URL: "postgres://u:p@localhost:5432/x"}}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "merchant_source") {
		t.Fatalf("unknown merchant_source must refuse to load, got %v", err)
	}
}

// or#893 phase 3: merchant_source=api must DECLARE where secrets live. Enabling
// Vault (which may only be Transit signing) no longer implies the KV store, and
// an undeclared backend refuses boot in every env — including development.
func TestMerchantSourceAPIRequiresAnExplicitSecretBackend(t *testing.T) {
	const wantExplicit = `merchant_source=api requires an explicit secret_backend ("db" or "vault"): where merchant secrets live is declared intent, never derived from vault.enabled (#661/#893)`

	for _, env := range []string{"production", "development"} {
		cfg := validAPIBase(env)
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), wantExplicit) {
			t.Fatalf("env %s: undeclared secret_backend must refuse boot, got %v", env, err)
		}

		// vault.enabled is NOT a declaration — it must still refuse.
		cfg = validAPIBase(env)
		cfg.Vault = &VaultConfig{Enabled: true}
		if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), wantExplicit) {
			t.Fatalf("env %s: vault.enabled must not stand in for secret_backend, got %v", env, err)
		}
	}

	// secret_backend=vault + a Vault connection → boots.
	cfg := validAPIBase("production")
	cfg.SecretBackend = SecretBackendVault
	cfg.Vault = &VaultConfig{Enabled: true}
	if err := Validate(cfg); err != nil {
		t.Fatalf("api mode with secret_backend=vault must validate: %v", err)
	}

	// secret_backend=db outside development needs ENCRYPTION_MASTER_KEY (#667).
	cfg = validAPIBase("production")
	cfg.SecretBackend = SecretBackendDB
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "merchant_source=api with secret_backend=db requires ENCRYPTION_MASTER_KEY outside development (#667/#723)") {
		t.Fatalf("secret_backend=db without a master key must refuse boot outside dev, got %v", err)
	}

	cfg = validAPIBase("production")
	cfg.SecretBackend = SecretBackendDB
	cfg.Encryption = &EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	if err := Validate(cfg); err != nil {
		t.Fatalf("api mode with secret_backend=db + master key must validate: %v", err)
	}

	// Development keeps the plaintext-DB posture.
	cfg = validAPIBase("development")
	cfg.SecretBackend = SecretBackendDB
	if err := Validate(cfg); err != nil {
		t.Fatalf("api mode in dev with secret_backend=db must validate: %v", err)
	}
}

func TestMerchantSourceAPIRefusesManifestEnvOverlays(t *testing.T) {
	t.Setenv("BILLING_MERCHANTS_ACME_DISPLAY_NAME", "Acme")
	cfg := validAPIBase("development")
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "two truths") {
		t.Fatalf("api mode with BILLING_MERCHANTS_* env must refuse boot, got %v", err)
	}
}

func TestMerchantSourceManifestIgnoresEncryptionPosture(t *testing.T) {
	// MODE 1 persists nothing; no ENCRYPTION_MASTER_KEY, no Vault — validates
	// even outside development (#723: the #667 gate is store-scoped).
	cfg := &Config{
		Env:               "production",
		MerchantSource:    MerchantSourceManifest,
		ProviderWriteMode: ProviderWriteModeReadOnly,
		DB:                &DBConfig{URL: "postgres://u:p@localhost:5432/x"},
		RateLimits:        GetDefaultBillingConfig().RateLimits,
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("manifest mode must not require an encryption/secret backend: %v", err)
	}
}
