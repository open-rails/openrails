package bootstrap

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
)

// DumpMerchantConfig reads a merchant's configuration from Vault, through
// directory, and returns it in the push-merchant-config YAML shape.
// Credentials (PSP and custodian secrets, the SCIM token, alert webhook URLs)
// are never exported, so alert webhooks are omitted; a redacted dump
// re-applies without a placeholder standing in for a real value. With a
// manifest as the configuration, the manifest is the dump.
func DumpMerchantConfig(ctx context.Context, cfg *billing.Config, directory *merchants.Service, slug string) (*BillingConfig, error) {
	if directory == nil {
		return nil, fmt.Errorf("dump-merchant-config requires the merchant configuration")
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, fmt.Errorf("merchant slug is required")
	}
	if config.MerchantConfigKVMount(cfg) == "" {
		return nil, fmt.Errorf("merchant configuration is read from the manifest; the manifest is the dump")
	}
	selected, err := directory.GetBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("lookup merchant %q: %w", slug, err)
	}
	set, err := directory.Config().Get(ctx, selected.ID)
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
	if set.HasMessages {
		messages := set.Messages.Value
		out.Messages = &messages
	}
	for _, key := range set.CustodianKeys() {
		c := set.Custodians[key].Value
		if out.Custodians == nil {
			out.Custodians = map[string]billing.CustodianConfig{}
		}
		out.Custodians[key] = billing.CustodianConfig{Kind: billing.CardVaultKind(c.Kind), Environment: billing.Environment(c.Environment), AccountID: c.AccountID, Archived: c.Archived, Settings: maps.Clone(c.Settings)}
	}
	for _, key := range set.PSPKeys() {
		p := set.PSPs[key].Value
		if out.PSPs == nil {
			out.PSPs = map[string]billing.PSPConfig{}
		}
		out.PSPs[key] = billing.PSPConfig{Rail: billing.Rail(p.Rail), Environment: billing.Environment(p.Environment), AccountID: p.AccountID, Archived: p.Archived, Custodian: p.Custodian, Settings: maps.Clone(p.Settings)}
	}
	return out
}

// MarshalMerchantManifest renders a config manifest to YAML, the canonical dump output.
func MarshalMerchantManifest(m *BillingConfig) ([]byte, error) {
	return yaml.Marshal(m)
}
