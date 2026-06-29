package embed

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/open-rails/openrails/internal/app"
	boot "github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ManifestMerchant is one merchant an embedder configures (#593): slug, display
// name, optional profile, and the provider_accounts[] it bills through. It
// aliases the internal manifest type so callers populate it WITHOUT importing
// internal/ — every field is exported, so embed.ManifestMerchant{Slug:...,
// ProviderAccounts:...} compiles in any consumer package.
type ManifestMerchant = boot.ManifestMerchant

// ManifestProviderAccount is one provider account (provider_type, environment,
// account_id, mode, secrets) attached to a ManifestMerchant. Post-#592 the
// account_id is operator-declared inline (e.g. NMI gateway id, CCBill acct/subacct).
type ManifestProviderAccount = boot.ManifestProviderAccount

// ManifestMerchantProfile is the merchant's display profile seeded into config.
type ManifestMerchantProfile = boot.ManifestMerchantProfile

// ManifestSecretSource declares ONE provider-account secret value (value/env/
// file/vault — exactly one). Provider-account secrets are stored seed-once.
type ManifestSecretSource = boot.ManifestSecretSource

// ManifestIssuer declares a host-app issuer trusted for a merchant. Meaningful
// only in standalone (control-plane) provisioning; billing-only embedded config
// ignores it (no AuthKit), so leave it nil here.
type ManifestIssuer = boot.ManifestIssuer

// UpsertMerchantConfig idempotently creates or updates a billing merchant and its
// provider accounts from the embedded engine. Run it as many times as you like —
// create-if-missing, reconcile-if-present — so an embedder (e.g. a legacy-data
// migrate) can just say "here are the payment providers for this merchant" on
// every run. Billing-only: it touches no AuthKit/control-plane state (the merchant
// is an ownerless billing bucket). Already-present secrets are left untouched. If
// the engine is not yet bound to a merchant, it binds to this one. Returns the
// merchant id.
func (rt *Runtime) UpsertMerchantConfig(ctx context.Context, m ManifestMerchant) (merchant.ID, error) {
	if rt == nil || rt.emb == nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: runtime not initialized")
	}
	a := rt.emb.App()
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: app database not initialized")
	}
	if err := validateCredentialMutationMode(a.CredentialMode, m); err != nil {
		return merchant.ID{}, err
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
		Merchant: m,
		Options:  boot.MerchantManifestReconcileOptions{Insert: true},
	}
	if len(m.ProviderAccounts) > 0 {
		backend, err := merchantsecrets.Build(ctx, conf, database.DataPool())
		if err != nil {
			return merchant.ID{}, fmt.Errorf("openrails embed: build secret store: %w", err)
		}
		req.SecretStore = backend.Secrets
	}

	tn, err := boot.ProvisionMerchant(ctx, req)
	if err != nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: upsert merchant config: %w", err)
	}
	if rt.tenantID.IsZero() {
		rt.tenantID = tn.ID
		a.Runtime.ConfiguredMerchant = tn.ID
	}
	return tn.ID, nil
}

func validateCredentialMutationMode(mode app.CredentialMode, m ManifestMerchant) error {
	if mode == app.CredentialModeMutable || len(m.ProviderAccounts) == 0 {
		return nil
	}
	return fmt.Errorf("openrails embed: provider account credential changes require mutable_credentials")
}

// ParseMerchantConfig parses a YAML merchant document into a ManifestMerchant.
func ParseMerchantConfig(raw []byte) (ManifestMerchant, error) {
	var m ManifestMerchant
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return ManifestMerchant{}, fmt.Errorf("openrails embed: parse merchant config: %w", err)
	}
	return m, nil
}
