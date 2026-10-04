package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/service"
)

type DumpMerchantConfigOptions struct {
	IncludeSecrets bool
}

// DumpMerchantConfig reads a merchant's OpenRails-owned configuration (identity,
// settings, custodians and PSPs)
// and returns it in the push-merchant-config YAML shape (#646/#653).
// Secret fields are omitted entirely by default (so a redacted dump can be
// re-applied without a placeholder overwriting real secrets). Plaintext export is refused.
func DumpMerchantConfig(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, slug string, opts DumpMerchantConfigOptions) (*BillingConfig, error) {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return nil, fmt.Errorf("dump-merchant-config requires an enabled control plane")
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, fmt.Errorf("merchant slug is required")
	}
	if opts.IncludeSecrets {
		return nil, fmt.Errorf("plaintext credential export is not supported by merchant configuration dump")
	}
	// Metadata is independent of credential custody and needs no secret backend.
	var secretStore merchants.MerchantSecretStore
	database, err := db.NewWithPGXPool(cp.Pool().Raw(), cp.Pool().Schema())
	if err != nil {
		return nil, fmt.Errorf("wrap control-plane db: %w", err)
	}

	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return nil, err
	}
	selected, err := directory.GetBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("lookup merchant %q: %w", slug, err)
	}
	slug = selected.Slug
	mid := selected.ID
	row, err := database.Gen(ctx).GetMerchantDirectoryByID(ctx, mid.UUID())
	if err != nil {
		return nil, err
	}
	displayName, apiHost := row.DisplayName, row.ApiHost
	mctx := merchant.WithID(ctx, mid)

	// merchants.display_name is the canonical merchant name (#041), NULL -> slug.
	mt := MerchantConfig{MerchantDeclaration: config.MerchantDeclaration{DisplayName: slug}}
	if displayName != nil && strings.TrimSpace(*displayName) != "" {
		mt.DisplayName = *displayName
	}
	if apiHost != nil && strings.TrimSpace(*apiHost) != "" {
		mt.APIHost = *apiHost
	}

	// Settings in the shape the configuration API reads and applies.
	if mt.Settings, err = service.ReadMerchantSettings(mctx, database); err != nil {
		return nil, fmt.Errorf("load merchant settings: %w", err)
	}

	// custodians (or#880) — dumped BEFORE the PSPs that reference them, and
	// keyed by row id so each PSP can emit its `custodian:` reference. Omitting
	// them would round-trip a custody arrangement into an unarmed one.
	var declaredCustodians []gen.BillingCustodian
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var lerr error
		declaredCustodians, lerr = database.Gen(ctx).ListCustodiansForMerchant(ctx, mid.UUID())
		return lerr
	}); err != nil {
		return nil, fmt.Errorf("list custodians: %w", err)
	}
	custodianSecrets, err := custodianSecretValues(ctx, secretStore, mid, opts.IncludeSecrets)
	if err != nil {
		return nil, err
	}
	custodianKeyByID := map[uuid.UUID]string{}
	if len(declaredCustodians) > 0 {
		mt.Custodians = map[string]CustodianConfig{}
	}
	for _, c := range declaredCustodians {
		key := strings.TrimSpace(c.Key)
		custodianKeyByID[c.ID] = key
		entry := CustodianConfig{Kind: c.Kind, AccountID: c.AccountID, Archived: c.Archived}
		var settings map[string]any
		if len(c.Settings) > 0 && json.Unmarshal(c.Settings, &settings) == nil && len(settings) > 0 {
			entry.Settings = settings
		}
		if values := custodianSecrets[custodianSecretGroupKey(c.Kind, c.Environment, c.AccountID)]; len(values) > 0 {
			entry.Secrets = values
		}
		mt.Custodians[key] = entry
	}

	// PSPs (identity + lifecycle + secret references).
	var accounts []gen.BillingPsp
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var lerr error
		accounts, lerr = database.Gen(ctx).ListPSPsForMerchant(ctx, mid.UUID())
		return lerr
	}); err != nil {
		return nil, fmt.Errorf("list PSPs: %w", err)
	}
	secretValuesByAccount, err2 := pspSecrets(ctx, secretStore, mid, opts.IncludeSecrets)
	if err2 != nil {
		return nil, err2
	}
	mt.PSPs = map[string]PSPConfig{}
	for _, a := range accounts {
		localKey := strings.TrimSpace(a.Key)
		if localKey == "" {
			localKey = pspDumpKey(a.Rail, a.Environment, a.AccountID)
		}
		account := PSPConfig{
			Rail:      billing.Rail(a.Rail),
			AccountID: a.AccountID,
			Archived:  a.Archived,
		}
		if a.CustodianID != nil {
			account.Custodian = custodianKeyByID[*a.CustodianID]
		}
		if signer := pspSignerFromRow(a.Signer); signer != nil {
			account.Signer = signer
		}
		if settings := pspSettingsFromRow(a.Settings); len(settings) > 0 {
			account.Settings = settings
		}
		key := pspSecretGroupKey(a.Rail, a.Environment, a.AccountID)
		if values := secretValuesByAccount[key]; len(values) > 0 {
			account.Secrets = values
		}
		mt.PSPs[localKey] = account
	}

	return &BillingConfig{Version: BootstrapManifestVersion, Merchants: map[string]MerchantConfig{slug: mt}}, nil
}

func pspSignerFromRow(raw []byte) *PSPSignerConfig {
	var signer struct {
		Mode string `json:"mode"`
		Key  string `json:"key"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &signer) != nil {
		return nil
	}
	mode := strings.TrimSpace(signer.Mode)
	if mode == "" || mode == "local_keypair" {
		return nil
	}
	return &PSPSignerConfig{Mode: mode, Key: strings.TrimSpace(signer.Key)}
}

func pspSettingsFromRow(raw []byte) map[string]any {
	var settings map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &settings) != nil || len(settings) == 0 {
		return nil
	}
	return settings
}

// MarshalMerchantManifest renders a config manifest to YAML, the canonical dump output.
func MarshalMerchantManifest(m *BillingConfig) ([]byte, error) {
	return yaml.Marshal(m)
}

// pspSecrets lists the merchant's PSP secret VALUES
// grouped by (rail, environment, account_id). It returns nothing unless
// includeValues is set: a redacted dump omits secret fields entirely rather than
// emitting a placeholder that a re-apply (--overwrite) could store as the real value.
func pspSecrets(ctx context.Context, secretStore merchants.MerchantSecretStore, mid billing.MerchantID, includeValues bool) (map[string]map[string]string, error) {
	if secretStore == nil || !includeValues {
		return nil, nil
	}
	names, err := secretStore.List(ctx, mid)
	if err != nil {
		return nil, fmt.Errorf("list merchant secrets: %w", err)
	}
	out := map[string]map[string]string{}
	for _, name := range names {
		rail, environment, accountID, key, ok, perr := merchants.ParsePSPSecretName(name)
		if perr != nil || !ok {
			continue
		}
		secret, err := secretStore.Get(ctx, mid, name)
		if err != nil {
			return nil, fmt.Errorf("read merchant secret %s for dump: %w", name, err)
		}
		gk := pspSecretGroupKey(rail, environment, accountID)
		if out[gk] == nil {
			out[gk] = map[string]string{}
		}
		out[gk][key] = secret.Value
	}
	return out, nil
}

func pspSecretGroupKey(rail, environment, accountID string) string {
	return strings.ToLower(rail) + "\x00" + strings.ToLower(environment) + "\x00" + accountID
}

// custodianSecretValues is the custody sibling of pspSecrets,
// grouped by the custodian's (kind, environment, account_id) identity.
func custodianSecretValues(ctx context.Context, secretStore merchants.MerchantSecretStore, mid billing.MerchantID, includeValues bool) (map[string]map[string]string, error) {
	if secretStore == nil || !includeValues {
		return nil, nil
	}
	names, err := secretStore.List(ctx, mid)
	if err != nil {
		return nil, fmt.Errorf("list merchant secrets: %w", err)
	}
	out := map[string]map[string]string{}
	for _, name := range names {
		kind, environment, accountID, key, ok, perr := merchants.ParseCustodianSecretName(name)
		if perr != nil || !ok {
			continue
		}
		secret, err := secretStore.Get(ctx, mid, name)
		if err != nil {
			return nil, fmt.Errorf("read merchant secret %s for dump: %w", name, err)
		}
		gk := custodianSecretGroupKey(kind, environment, accountID)
		if out[gk] == nil {
			out[gk] = map[string]string{}
		}
		out[gk][key] = secret.Value
	}
	return out, nil
}

func custodianSecretGroupKey(kind, environment, accountID string) string {
	return strings.ToLower(kind) + "\x00" + strings.ToLower(environment) + "\x00" + accountID
}

func pspDumpKey(rail, environment, accountID string) string {
	parts := []string{rail, environment, accountID}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('-')
		}
		for _, r := range strings.ToLower(p) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			} else {
				b.WriteByte('-')
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
