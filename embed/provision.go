package embed

import (
	"context"
	"fmt"

	"github.com/goccy/go-yaml"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/signeridentity"
	"github.com/open-rails/openrails/pkg/merchant"
)

type BillingConfig = boot.BillingConfig
type MerchantConfig = boot.MerchantConfig

// InvoiceConfig aliases the merchant invoice/collection policy block so
// embedded hosts can set it programmatically (#798).
type InvoiceConfig = boot.InvoiceConfig
type MerchantProfileConfig = boot.MerchantProfileConfig
type PSPConfig = boot.PSPConfig

// CheckoutRoutingRuleConfig / CheckoutRoutingMatchConfig alias the or#288
// processor-routing policy block so embedded hosts can declare it
// programmatically alongside their PSPs.
type CheckoutRoutingRuleConfig = boot.CheckoutRoutingRuleConfig
type CheckoutRoutingMatchConfig = boot.CheckoutRoutingMatchConfig

type ProviderRailAccountConfig = boot.ProviderRailAccountConfig
type PSPSignerConfig = boot.PSPSignerConfig

// upsertMerchantConfig reconciles the constructor's merchant declaration before
// HTTP routes or workers can observe a partially configured runtime. With
// tolerateVault, a Solana vault_transit signer whose Vault is unavailable
// reuses the identity stored for it, or (none stored yet) leaves only that PSP
// unprovisioned; the returned flag says Vault must still confirm it.
func upsertMerchantConfig(ctx context.Context, a *app.App, slug string, m MerchantConfig, tolerateVault bool) (merchant.ID, bool, error) {
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return merchant.ID{}, false, fmt.Errorf("openrails embed: app database not initialized")
	}
	conf := a.Config
	if conf == nil || conf.DB == nil {
		return merchant.ID{}, false, fmt.Errorf("openrails embed: config/db is required")
	}
	database := a.Runtime.DB

	directory := a.Runtime.Merchants
	if directory == nil {
		var err error
		directory, err = merchants.NewDirectoryService(database.DataPool())
		if err != nil {
			return merchant.ID{}, false, err
		}
	}
	bound := a.Runtime.ConfiguredMerchant()
	if !bound.IsZero() {
		selected, err := directory.GetBySlug(ctx, merchant.NormalizeSlug(slug))
		if err != nil {
			return merchant.ID{}, false, fmt.Errorf("openrails embed: engine is already bound to merchant %s; cannot resolve supplied name %q: %w", bound, slug, err)
		}
		if selected.ID != bound {
			return merchant.ID{}, false, fmt.Errorf("openrails embed: one embedded engine serves one merchant; refusing second merchant %q (%s), bound to %s", slug, selected.ID, bound)
		}
		slug = selected.Slug
	}

	// Run it through the same billing-only merchant-provisioning boundary embed.New
	// and the bootstrap CLI use, with ControlPlane nil. Provider-account reconcile
	// needs a merchant secret store (built over the engine's own pool).
	req := boot.ProvisionMerchantRequest{
		Config:     conf,
		Database:   database,
		MerchantID: bound,
		Directory:  directory,
		Slug:       slug,
		Merchant:   m,
		Options:    boot.MerchantManifestReconcileOptions{Insert: true, StripeClients: a.Runtime.StripeClients},
	}
	var fallback *signeridentity.Transit
	switch {
	case conf.SecretStoreBackend() == config.SecretBackendSnapshot:
		// Load the host-owned snapshot into this process. Metadata initialization
		// is create-only; authorized edits and archived accounts survive restarts.
		if a.Runtime == nil || a.Runtime.ManifestSecrets == nil {
			return merchant.ID{}, false, fmt.Errorf("openrails embed: snapshot credentials require the runtime snapshot plane")
		}
		req.SecretStore = a.Runtime.ManifestSecrets.Seeder()
		backend := a.Runtime.MerchantSecretBackend
		if backend == nil {
			return merchant.ID{}, false, fmt.Errorf("openrails embed: credential backend must be initialized before provisioning")
		}
		req.SolanaTransit = backend.SolanaTransit
		if backend.SolanaTransit != nil {
			fallback = &signeridentity.Transit{TransitClient: backend.SolanaTransit, DB: database, Directory: directory, Slug: slug,
				Environment: config.ExpectedProviderEnvironment(conf.IsTestMode()), Tolerate: tolerateVault, OnChange: a.Runtime.ReportSignerKeyChange}
			req.SolanaTransit = fallback
			req.Options.DeferPSP = fallback.Defers
		}
	case len(m.PSPs) > 0 || len(m.Custodians) > 0:
		return merchant.ID{}, false, fmt.Errorf("openrails embed: provider credential declarations require a host snapshot; use the Client payment-provider publication operation for managed credentials")
	}

	tn, err := boot.ProvisionMerchant(ctx, req)
	if err != nil {
		return merchant.ID{}, false, fmt.Errorf("openrails embed: upsert merchant config: %w", err)
	}
	if a.Runtime.ConfiguredMerchant().IsZero() {
		a.Runtime.SetConfiguredMerchant(tn.ID)
	}
	return tn.ID, fallback != nil && fallback.Unavailable(), nil
}

// approveSolanaSigner accepts the identity Vault now reports for key and
// re-applies the declaration, provisioning it.
func approveSolanaSigner(ctx context.Context, a *app.App, declaration *MerchantDeclaration, mid merchant.ID, key string) error {
	if declaration == nil {
		return fmt.Errorf("openrails embed: no merchant declaration to approve a signer for")
	}
	backend := a.Runtime.MerchantSecretBackend
	if backend == nil {
		return fmt.Errorf("openrails embed: no Vault Transit signer is configured")
	}
	directory, err := merchants.NewDirectoryService(a.Runtime.DB.DataPool())
	if err != nil {
		return err
	}
	environment := config.ExpectedProviderEnvironment(a.Config.IsTestMode())
	if _, err := signeridentity.Approve(ctx, a.Runtime.DB, directory, backend.SolanaTransit, mid, environment, key); err != nil {
		return fmt.Errorf("openrails embed: %w", err)
	}
	if _, _, err := upsertMerchantConfig(ctx, a, declaration.Slug, declaration.Config, false); err != nil {
		return err
	}
	a.Runtime.ReportSignerKeyChange(nil)
	return nil
}

// ParseMerchantConfig parses a single merchant YAML document into a MerchantConfig.
// Strict (#711): unknown fields are rejected — the same DisallowUnknownField
// posture as the manifest paths — so a typo'd `acounts:` fails loudly instead
// of silently provisioning a merchant with no rails.
func ParseMerchantConfig(raw []byte) (MerchantConfig, error) {
	// Renamed keys get a pointer instead of a bare "unknown field" (#698/#683).
	var probe map[string]any
	if yaml.Unmarshal(raw, &probe) == nil {
		for _, old := range []string{"rail_merchant_accounts", "provider_accounts"} {
			if _, ok := probe[old]; ok {
				return MerchantConfig{}, fmt.Errorf("openrails embed: parse merchant config: %s was renamed to psps", old)
			}
		}
	}
	var m MerchantConfig
	if err := yaml.UnmarshalWithOptions(raw, &m, yaml.DisallowUnknownField()); err != nil {
		return MerchantConfig{}, fmt.Errorf("openrails embed: parse merchant config: %w", err)
	}
	return m, nil
}

// ParseMerchantConfigManifest parses a multi-merchant config manifest.
func ParseMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	return boot.ParseMerchantConfigManifest(raw)
}

// LoadMerchantConfigManifestWithOverlays merges the manifest with the host's
// structured YAML secret overlays (later wins) and validates the result. The
// host's config tree is the only source; the engine reads no env or files.
func LoadMerchantConfigManifestWithOverlays(raw []byte, overlays ...[]byte) (*BillingConfig, error) {
	return boot.LoadMerchantConfigManifestWithOverlays(raw, overlays...)
}
