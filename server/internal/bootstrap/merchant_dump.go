package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/goccy/go-yaml"
	vaultapi "github.com/hashicorp/vault/api"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// DumpMerchantConfig reads a merchant's configuration from Vault and returns
// it in the push-merchant-config YAML shape (#646/#653). Credentials (PSP and
// custodian secrets, the SCIM token, alert webhook URLs) are never exported,
// so alert webhooks are omitted; a redacted dump re-applies without a
// placeholder standing in for a real value. With a manifest as the
// configuration, the manifest is the dump.
func DumpMerchantConfig(ctx context.Context, logger *slog.Logger, cfg *config.Config, vault *vaultapi.Client, cp *controlplane.ControlPlane, slug string) (*BillingConfig, error) {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return nil, fmt.Errorf("dump-merchant-config requires an enabled control plane")
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, fmt.Errorf("merchant slug is required")
	}
	if config.MerchantConfigKVMount(cfg) == "" {
		return nil, fmt.Errorf("merchant configuration is read from the manifest; the manifest is the dump")
	}
	database, err := db.NewWithPGXPool(logger, cp.Pool().Raw(), cp.Pool().Schema())
	if err != nil {
		return nil, fmt.Errorf("wrap control-plane db: %w", err)
	}
	opened, err := merchantbootstrap.OpenMerchants(ctx, cfg, database, vault)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	svc := opened.Service
	selected, err := svc.GetBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("lookup merchant %q: %w", slug, err)
	}
	set, err := svc.Config().Get(ctx, selected.ID)
	if err != nil {
		return nil, err
	}
	return &BillingConfig{Version: merchantbootstrap.BootstrapManifestVersion, Merchants: map[string]MerchantConfig{
		selected.Slug: {MerchantDocument: dumpDeclaration(set, selected.Slug)},
	}}, nil
}

// dumpDeclaration is a configuration as its manifest declares it: its
// documents, without credentials.
func dumpDeclaration(set merchantdocs.Set, slug string) billing.MerchantDocument {
	doc := set.Merchant.Value
	out := billing.MerchantDocument{DisplayName: doc.DisplayName, MerchantSettings: doc.MerchantSettings}
	if out.DisplayName == "" {
		out.DisplayName = slug
	}
	for _, key := range set.CustodianKeys() {
		c := set.Custodians[key].Value
		if out.Custodians == nil {
			out.Custodians = map[string]billing.CustodianConfig{}
		}
		out.Custodians[key] = billing.CustodianConfig{Kind: c.Kind, Environment: c.Environment, AccountID: c.AccountID, Archived: c.Archived, Settings: maps.Clone(c.Settings)}
	}
	for _, key := range set.PSPKeys() {
		p := set.PSPs[key].Value
		if out.PSPs == nil {
			out.PSPs = map[string]billing.PSPConfig{}
		}
		out.PSPs[key] = billing.PSPConfig{Rail: billing.Rail(p.Rail), Environment: p.Environment, AccountID: p.AccountID, Archived: p.Archived, Custodian: p.Custodian, Settings: maps.Clone(p.Settings)}
	}
	return out
}

// MarshalMerchantManifest renders a config manifest to YAML, the canonical dump output.
func MarshalMerchantManifest(m *BillingConfig) ([]byte, error) {
	return yaml.Marshal(m)
}
