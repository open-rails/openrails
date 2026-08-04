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

func TestMerchantSourceAPIRequiresSecretBackendOutsideDev(t *testing.T) {
	// No Vault, no master key, production → refuse.
	cfg := validAPIBase("production")
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "merchant-secret backend") {
		t.Fatalf("api mode without a secret backend must refuse boot outside dev, got %v", err)
	}

	// Vault enabled → boots.
	cfg = validAPIBase("production")
	cfg.Vault = &VaultConfig{Enabled: true}
	if err := Validate(cfg); err != nil {
		t.Fatalf("api mode with Vault must validate: %v", err)
	}

	// ENCRYPTION_MASTER_KEY (DB store) → boots.
	cfg = validAPIBase("production")
	cfg.Encryption = &EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	if err := Validate(cfg); err != nil {
		t.Fatalf("api mode with master key must validate: %v", err)
	}

	// Development: no backend required.
	cfg = validAPIBase("development")
	if err := Validate(cfg); err != nil {
		t.Fatalf("api mode in dev must validate without a backend: %v", err)
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
