package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DumpMerchantConfig reads a merchant's OpenRails-owned configuration (identity,
// profile, invoice/collection policy, delegated-invoker windows, and provider
// accounts with secrets as REFERENCES) and returns it in the push-merchant-config
// manifest shape (#646). The Go struct family is the single source of truth for the
// YAML shape, so push and dump are symmetric. Secret VALUES are never emitted —
// only `env:` placeholder references derived from the canonical secret name — so a
// dump is safe to commit/version and is re-pushable once the operator supplies the
// referenced env values. Issuer trust lives in AuthKit's remote_application
// registry (#480/#481), not an OpenRails table, so it is not part of the dump.
func DumpMerchantConfig(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, slug string) (*MerchantManifest, error) {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return nil, fmt.Errorf("dump-merchant-config requires an enabled control plane")
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, fmt.Errorf("merchant slug is required")
	}
	secretBackend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	if err != nil {
		return nil, fmt.Errorf("build secret store: %w", err)
	}
	secretStore := secretBackend.Secrets
	database, err := db.NewWithPGXPool(cp.Pool().Raw(), cp.Pool().Schema())
	if err != nil {
		return nil, fmt.Errorf("wrap control-plane db: %w", err)
	}

	var merchantID string
	var displayName *string
	if err := database.Qx(ctx).QueryRow(ctx, `
		SELECT id::text, display_name FROM openrails.merchants WHERE slug = $1
	`, slug).Scan(&merchantID, &displayName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("merchant %q not found", slug)
		}
		return nil, fmt.Errorf("lookup merchant %q: %w", slug, err)
	}
	mid, err := merchant.ParseID(merchantID)
	if err != nil {
		return nil, fmt.Errorf("parse merchant id: %w", err)
	}
	mctx := merchant.WithID(ctx, mid)

	// merchants.display_name is the canonical merchant name (#041), NULL → slug.
	mt := ManifestMerchant{Slug: slug, DisplayName: slug}
	if displayName != nil && strings.TrimSpace(*displayName) != "" {
		mt.DisplayName = *displayName
	}

	// merchant_configurations payload: profile + invoice + delegated-invoker windows.
	conf, found, err := merchantconfig.NewStore(database).Get(mctx)
	if err != nil {
		return nil, fmt.Errorf("load merchant configuration: %w", err)
	}
	if found {
		mt.Profile = ManifestMerchantProfile{
			DisplayName: conf.Profile.DisplayName,
			LogoURL:     conf.Profile.LogoURL,
			FromEmail:   conf.Profile.FromEmail,
			SupportURL:  conf.Profile.SupportURL,
		}
		if conf.InvoiceCollectionThreshold != nil || conf.InvoiceMonthlyFloor != nil || strings.TrimSpace(conf.InvoiceBillingBoundary) != "" {
			mt.Invoice = &ManifestInvoiceConfig{
				CollectionThreshold:   conf.InvoiceCollectionThreshold,
				MonthlyFloor:          conf.InvoiceMonthlyFloor,
				BillingPeriodBoundary: strings.TrimSpace(conf.InvoiceBillingBoundary),
			}
		}
		for _, w := range conf.DelegatedInvokerWastedSpendWindows {
			if w.WindowSeconds <= 0 {
				continue
			}
			mt.DelegatedInvokerWastedSpendWindows = append(mt.DelegatedInvokerWastedSpendWindows, ManifestBudgetWindow{
				Key:      w.Key,
				Window:   formatWindowSeconds(w.WindowSeconds),
				Limit:    w.Limit,
				Currency: w.Currency,
			})
		}
	}

	// provider accounts (identity + role + secret references).
	var accounts []gen.OpenrailsProviderAccount
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var lerr error
		accounts, lerr = database.Gen(ctx).ListProviderAccountsForMerchant(ctx, gen.ListProviderAccountsForMerchantParams{
			MerchantID: mid.UUID(),
		})
		return lerr
	}); err != nil {
		return nil, fmt.Errorf("list provider accounts: %w", err)
	}
	secretKeysByAccount, err := providerAccountSecretKeys(ctx, secretStore, mid)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		name := ""
		if a.DisplayName != nil {
			name = *a.DisplayName
		}
		account := ManifestProviderAccount{
			ProviderType: a.ProviderType,
			Name:         name,
			Environment:  a.Environment,
			AccountID:    a.AccountID,
			Mode:         providerAccountModeFromRoleStatus(a.Role, a.Status),
		}
		if a.VaultSecretRef != nil {
			account.VaultSecretRef = *a.VaultSecretRef
		}
		key := providerAccountSecretGroupKey(a.ProviderType, a.Environment, a.AccountID)
		if keys := secretKeysByAccount[key]; len(keys) > 0 {
			account.Secrets = map[string]ManifestSecretSource{}
			for _, k := range keys {
				account.Secrets[k] = ManifestSecretSource{Env: secretEnvPlaceholder(slug, a.ProviderType, a.Environment, a.AccountID, k)}
			}
		}
		mt.ProviderAccounts = append(mt.ProviderAccounts, account)
	}

	return &MerchantManifest{Version: BootstrapManifestVersion, Merchants: []ManifestMerchant{mt}}, nil
}

// MarshalMerchantManifest renders a manifest to YAML — the canonical dump output.
func MarshalMerchantManifest(m *MerchantManifest) ([]byte, error) {
	return yaml.Marshal(m)
}

func providerAccountModeFromRoleStatus(role, status string) string {
	if strings.EqualFold(status, "disabled") {
		return "disabled"
	}
	switch strings.ToLower(role) {
	case configRailRolePrimary:
		return configRailRolePrimary
	case configRailRoleLegacy:
		return configRailRoleLegacy
	default:
		return configRailRoleSecondary
	}
}

// providerAccountSecretKeys lists the merchant's secrets and groups the
// provider-account secret KEYS by (provider, environment, account_id).
func providerAccountSecretKeys(ctx context.Context, secretStore merchants.MerchantSecretStore, mid merchant.ID) (map[string][]string, error) {
	if secretStore == nil {
		return nil, nil
	}
	names, err := secretStore.List(ctx, mid)
	if err != nil {
		return nil, fmt.Errorf("list merchant secrets: %w", err)
	}
	out := map[string][]string{}
	for _, name := range names {
		providerType, environment, accountID, key, ok, perr := merchants.ParseProviderAccountSecretName(name)
		if perr != nil || !ok {
			continue
		}
		gk := providerAccountSecretGroupKey(providerType, environment, accountID)
		out[gk] = append(out[gk], key)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out, nil
}

func providerAccountSecretGroupKey(providerType, environment, accountID string) string {
	return strings.ToLower(providerType) + "\x00" + strings.ToLower(environment) + "\x00" + accountID
}

// secretEnvPlaceholder derives a stable, legible env-var name for a dumped secret
// reference (the value is never dumped). Operators wire the env var, then re-push.
func secretEnvPlaceholder(slug, providerType, environment, accountID, key string) string {
	parts := []string{slug, providerType, environment, accountID, key}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('_')
		}
		for _, r := range strings.ToUpper(p) {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
	}
	return b.String()
}

// formatWindowSeconds renders a window duration as the shortest clean unit string.
func formatWindowSeconds(seconds int64) string {
	switch {
	case seconds%int64(time.Hour/time.Second) == 0:
		return fmt.Sprintf("%dh", seconds/int64(time.Hour/time.Second))
	case seconds%int64(time.Minute/time.Second) == 0:
		return fmt.Sprintf("%dm", seconds/int64(time.Minute/time.Second))
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}
