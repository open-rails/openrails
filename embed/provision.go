package embed

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	boot "github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/pkg/merchant"
)

type BillingConfig = boot.BillingConfig
type MerchantConfig = boot.MerchantConfig
type MerchantProfileConfig = boot.MerchantProfileConfig
type RailMerchantAccountConfig = boot.RailMerchantAccountConfig
type ProviderRailAccountConfig = boot.ProviderRailAccountConfig
type RailMerchantAccountSignerConfig = boot.RailMerchantAccountSignerConfig
type RemoteApplicationConfig = boot.RemoteApplicationConfig
type StaticJWKSConfig = boot.StaticJWKSConfig
type StaticJWKConfig = boot.StaticJWKConfig

// UpsertMerchantConfig idempotently creates or updates a billing merchant and its
// provider accounts from the embedded engine. Run it as many times as you like —
// create-if-missing, reconcile-if-present — so an embedder (e.g. a legacy-data
// migrate) can just say "here are the payment providers for this merchant" on
// every run. Billing-only: it touches no AuthKit/control-plane state (the merchant
// is an ownerless billing bucket). Already-present secrets are left untouched. If
// the engine is not yet bound to a merchant, it binds to this one. Returns the
// merchant id.
func (rt *Runtime) UpsertMerchantConfig(ctx context.Context, slug string, m MerchantConfig) (merchant.ID, error) {
	if rt == nil || rt.emb == nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: runtime not initialized")
	}
	a := rt.emb.App()
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: app database not initialized")
	}
	conf := rt.emb.Config()
	if conf == nil || conf.DB == nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: config/db is required")
	}
	database := a.Runtime.DB

	// Run it through the same billing-only merchant-provisioning boundary embed.New
	// and the bootstrap CLI use, with ControlPlane nil. Provider-account reconcile
	// needs a merchant secret store (built over the engine's own pool).
	req := boot.ProvisionMerchantRequest{
		Config:   conf,
		Database: database,
		Slug:     slug,
		Merchant: m,
		Options:  boot.MerchantManifestReconcileOptions{Insert: true},
	}
	if len(m.RailMerchantAccounts) > 0 {
		backend, err := merchantsecrets.Build(ctx, conf, database.DataPool())
		if err != nil {
			return merchant.ID{}, fmt.Errorf("openrails embed: build secret store: %w", err)
		}
		req.SecretStore = backend.Secrets
		// #661: gate the embedded route surface on what OpenRails can actually do
		// (same process-global Vault token). Advisory — hides/degrades, not authz.
		if a.Runtime != nil {
			a.Runtime.RouteCapabilities = &routesurface.RuntimeCapabilities{
				SolanaCanSign: backend.SolanaCanSign,
				SecretWrite:   backend.SecretWrite,
			}
			if !backend.SolanaCanSign && a.Runtime.Rails.GetSolanaRail() != nil {
				log.Warn("solana: a Solana rail is configured but OpenRails cannot sign (no Vault connection and no local key); Solana signing routes are disabled")
			}
		}
	}

	tn, err := boot.ProvisionMerchant(ctx, req)
	if err != nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: upsert merchant config: %w", err)
	}
	if a.Runtime.ConfiguredMerchant.IsZero() {
		a.Runtime.ConfiguredMerchant = tn.ID
	}
	return tn.ID, nil
}

// ParseMerchantConfig parses a single merchant YAML document into a MerchantConfig.
func ParseMerchantConfig(raw []byte) (MerchantConfig, error) {
	var m MerchantConfig
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return MerchantConfig{}, fmt.Errorf("openrails embed: parse merchant config: %w", err)
	}
	return m, nil
}

// ParseMerchantConfigManifest parses a multi-merchant config manifest.
func ParseMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	return boot.ParseMerchantConfigManifest(raw)
}

// LoadMerchantConfigManifest parses a multi-merchant config manifest and applies
// BILLING_ environment overlays using OpenRails' merchant config mapper.
func LoadMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	return boot.LoadMerchantConfigManifestBytes(raw)
}
