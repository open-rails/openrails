package embed

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"

	boot "github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantsecrets"
)

// MerchantConfig is the declarative billing configuration for one merchant
// (#593): its display profile and the rail provider accounts it bills through.
// It is applied idempotently by (*Runtime).UpsertMerchantConfig — the public,
// embedded-host entry point for the provisioning that previously lived only
// behind the CLI and the embed.New construction side-effect.
type MerchantConfig struct {
	Profile          *MerchantProfile  `yaml:"profile,omitempty"`
	ProviderAccounts []ProviderAccount `yaml:"provider_accounts,omitempty"`
}

// MerchantProfile is the merchant's display/contact profile.
type MerchantProfile struct {
	DisplayName string `yaml:"display_name,omitempty"`
	LogoURL     string `yaml:"logo_url,omitempty"`
	FromEmail   string `yaml:"from_email,omitempty"`
	SupportURL  string `yaml:"support_url,omitempty"`
}

// ProviderAccount declares one rail account a merchant bills through. Post-#592,
// account_id is operator-declared (no runtime whoami / verification). Secrets are
// the rail credentials, stored per-merchant; they are seed-once (an existing
// secret is left untouched, never reverted to this declaration).
type ProviderAccount struct {
	ProviderType   string                  `yaml:"provider_type"`
	Environment    string                  `yaml:"environment,omitempty"` // "live" (default) | "test"
	AccountID      string                  `yaml:"account_id,omitempty"`
	Mode           string                  `yaml:"mode,omitempty"`
	VaultSecretRef string                  `yaml:"vault_secret_ref,omitempty"`
	Secrets        map[string]SecretSource `yaml:"secrets,omitempty"`
}

// SecretSource resolves one credential value from an inline value, env var,
// file, or Vault path (exactly one).
type SecretSource struct {
	Value string `yaml:"value,omitempty"`
	Env   string `yaml:"env,omitempty"`
	File  string `yaml:"file,omitempty"`
	Vault string `yaml:"vault,omitempty"`
}

// MerchantConfigResult reports what UpsertMerchantConfig provisioned.
type MerchantConfigResult struct {
	MerchantID       string
	Slug             string
	ProviderAccounts int
}

// ParseMerchantConfig parses a YAML merchant-config document into a MerchantConfig.
func ParseMerchantConfig(raw []byte) (MerchantConfig, error) {
	var m MerchantConfig
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return MerchantConfig{}, fmt.Errorf("openrails embed: parse merchant config: %w", err)
	}
	return m, nil
}

// UpsertMerchantConfig idempotently provisions the billing merchant `slug` and
// the provider accounts in cfg, creating the merchant if missing. It is the
// public, declarative provisioning call an embedded host (e.g. a migrator) uses
// to say "here are the payment providers for this merchant" — wrapping the same
// billing-only ProvisionMerchant boundary embed.New uses, so no AuthKit/control
// plane is required. Seed-once: an already-present secret is not overwritten.
func (rt *Runtime) UpsertMerchantConfig(ctx context.Context, slug string, cfg MerchantConfig) (MerchantConfigResult, error) {
	if rt == nil || rt.emb == nil {
		return MerchantConfigResult{}, fmt.Errorf("openrails embed: runtime not initialized")
	}
	a := rt.emb.App()
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return MerchantConfigResult{}, fmt.Errorf("openrails embed: app database not initialized")
	}
	conf := rt.emb.Config()
	if conf == nil || conf.DB == nil {
		return MerchantConfigResult{}, fmt.Errorf("openrails embed: config/db is required")
	}

	// Provider-account reconcile needs a merchant secret store (required even for
	// secret-less accounts). Build it over the same pool the engine runs on.
	pool := db.WrapPool(a.Runtime.DB.Pool(), conf.DB.SchemaName())
	secretBackend, err := merchantsecrets.Build(ctx, conf, pool)
	if err != nil {
		return MerchantConfigResult{}, fmt.Errorf("openrails embed: build secret store: %w", err)
	}

	tn, err := boot.ProvisionMerchant(ctx, boot.ProvisionMerchantRequest{
		Config:      conf,
		Database:    a.Runtime.DB,
		SecretStore: secretBackend.Secrets,
		Merchant:    manifestMerchantFromConfig(slug, cfg),
		Options:     boot.MerchantManifestReconcileOptions{Insert: true},
	})
	if err != nil {
		return MerchantConfigResult{}, fmt.Errorf("openrails embed: upsert merchant config: %w", err)
	}

	// Bind the engine to this merchant if it isn't already (mirrors embed.New).
	if rt.tenantID.IsZero() {
		rt.tenantID = tn.ID
		a.Runtime.ConfiguredMerchant = tn.ID
	}
	return MerchantConfigResult{MerchantID: tn.ID.String(), Slug: tn.Slug, ProviderAccounts: len(cfg.ProviderAccounts)}, nil
}

// manifestMerchantFromConfig maps the public MerchantConfig onto the internal
// bootstrap manifest shape ProvisionMerchant consumes.
func manifestMerchantFromConfig(slug string, cfg MerchantConfig) boot.ManifestMerchant {
	displayName := slug
	var profile boot.ManifestMerchantProfile
	if cfg.Profile != nil {
		profile = boot.ManifestMerchantProfile{
			DisplayName: cfg.Profile.DisplayName,
			LogoURL:     cfg.Profile.LogoURL,
			FromEmail:   cfg.Profile.FromEmail,
			SupportURL:  cfg.Profile.SupportURL,
		}
		if profile.DisplayName != "" {
			displayName = profile.DisplayName
		}
	}
	accounts := make([]boot.ManifestProviderAccount, 0, len(cfg.ProviderAccounts))
	for _, pa := range cfg.ProviderAccounts {
		secrets := make(map[string]boot.ManifestSecretSource, len(pa.Secrets))
		for k, s := range pa.Secrets {
			secrets[k] = boot.ManifestSecretSource{Value: s.Value, Env: s.Env, File: s.File, Vault: s.Vault}
		}
		accounts = append(accounts, boot.ManifestProviderAccount{
			ProviderType:   pa.ProviderType,
			Environment:    pa.Environment,
			AccountID:      pa.AccountID,
			Mode:           pa.Mode,
			VaultSecretRef: pa.VaultSecretRef,
			Secrets:        secrets,
		})
	}
	return boot.ManifestMerchant{Slug: slug, DisplayName: displayName, Profile: profile, ProviderAccounts: accounts}
}
