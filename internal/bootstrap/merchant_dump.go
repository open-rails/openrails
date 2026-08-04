package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type DumpMerchantConfigOptions struct {
	IncludeSecrets bool
}

// DumpMerchantConfig reads a merchant's OpenRails-owned configuration (identity,
// profile, invoice/collection policy, delegated-invoker windows, and provider
// accounts) and returns it in the push-merchant-config YAML shape (#646/#653).
// Secret fields are omitted entirely by default (so a redacted dump can be
// re-applied without a placeholder overwriting real secrets); IncludeSecrets
// emits plaintext from the configured secret backend for operator-controlled exports.
func DumpMerchantConfig(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, slug string, opts DumpMerchantConfigOptions) (*BillingConfig, error) {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return nil, fmt.Errorf("dump-merchant-config requires an enabled control plane")
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, fmt.Errorf("merchant slug is required")
	}
	// #723: in manifest mode there is no store to dump — the YAML the operator
	// already holds IS the truth (DB rows are projections of it).
	if cfg.IsManifestMerchantSource() {
		return nil, fmt.Errorf("merchant_source=manifest has no merchant-secret store to dump (#723): the boot manifest is already the export; dump-merchant-config serves merchant_source=api deployments")
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
	var displayName, apiHost *string
	if err := database.Qx(ctx).QueryRow(ctx, `
		SELECT id::text, display_name, api_host FROM openrails.merchants WHERE slug = $1
	`, slug).Scan(&merchantID, &displayName, &apiHost); err != nil {
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

	// merchants.display_name is the canonical merchant name (#041), NULL -> slug.
	mt := MerchantConfig{DisplayName: slug}
	if displayName != nil && strings.TrimSpace(*displayName) != "" {
		mt.DisplayName = *displayName
	}
	if apiHost != nil && strings.TrimSpace(*apiHost) != "" {
		mt.APIHost = *apiHost
	}

	// merchant_configurations payload: profile + invoice + delegated-invoker windows.
	conf, found, err := merchantconfig.NewStore(database).Get(mctx)
	if err != nil {
		return nil, fmt.Errorf("load merchant configuration: %w", err)
	}
	if found {
		mt.Profile = MerchantProfileConfig{
			DisplayName: conf.Profile.DisplayName,
			LogoURL:     conf.Profile.LogoURL,
			FromEmail:   conf.Profile.FromEmail,
			SupportURL:  conf.Profile.SupportURL,
			SignupURL:   conf.Profile.SignupURL,
		}
		if conf.InvoiceCollectionThreshold != nil || conf.InvoiceMonthlyFloor != nil ||
			strings.TrimSpace(conf.InvoiceBillingBoundary) != "" ||
			conf.ArrearsGraceDays != nil || conf.ArrearsDelinquencyFloor != nil {
			mt.Invoice = &InvoiceConfig{
				CollectionThreshold:    conf.InvoiceCollectionThreshold,
				MonthlyFloor:           conf.InvoiceMonthlyFloor,
				BillingPeriodBoundary:  strings.TrimSpace(conf.InvoiceBillingBoundary),
				DelinquencyGraceDays:   conf.ArrearsGraceDays,
				DelinquencyAmountFloor: conf.ArrearsDelinquencyFloor,
			}
		}
		for _, w := range conf.DelegatedInvokerWastedSpendWindows {
			if w.WindowSeconds <= 0 {
				continue
			}
			mt.DelegatedInvokerWastedSpendWindows = append(mt.DelegatedInvokerWastedSpendWindows, BudgetWindowConfig{
				Key:      w.Key,
				Window:   formatWindowSeconds(w.WindowSeconds),
				Limit:    w.Limit,
				Currency: w.Currency,
			})
		}
	}

	// provider accounts (identity + lifecycle + secret references).
	var accounts []gen.OpenrailsPsp
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var lerr error
		accounts, lerr = database.Gen(ctx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{
			MerchantID: mid.UUID(),
		})
		return lerr
	}); err != nil {
		return nil, fmt.Errorf("list provider accounts: %w", err)
	}
	secretValuesByAccount, err := railMerchantAccountSecrets(ctx, secretStore, mid, opts.IncludeSecrets)
	if err != nil {
		return nil, err
	}
	mt.PSPs = map[string]PSPConfig{}
	for _, a := range accounts {
		localKey := ""
		if a.Key != nil {
			localKey = strings.TrimSpace(*a.Key)
		}
		if localKey == "" {
			localKey = railMerchantAccountDumpKey(a.Rail, a.Environment, a.AccountID)
		}
		account := ProviderRailAccountConfig{
			Environment: a.Environment,
			AccountID:   a.AccountID,
			Archived:    a.Archived,
		}
		if signer := railMerchantAccountSignerFromEvidence(a.Evidence); signer != nil {
			account.Signer = signer
		}
		if settings := railMerchantAccountSettingsFromEvidence(a.Evidence); len(settings) > 0 {
			account.Settings = settings
		}
		key := railMerchantAccountSecretGroupKey(a.Rail, a.Environment, a.AccountID)
		if values := secretValuesByAccount[key]; len(values) > 0 {
			account.Secrets = values
		}
		mt.PSPs[localKey] = PSPConfig{a.Rail: account}
	}

	return &BillingConfig{Version: BootstrapManifestVersion, Merchants: map[string]MerchantConfig{slug: mt}}, nil
}

func railMerchantAccountSignerFromEvidence(raw []byte) *RailMerchantAccountSignerConfig {
	var evidence struct {
		Signer struct {
			Mode string `json:"mode"`
			Key  string `json:"key"`
		} `json:"signer"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &evidence) != nil {
		return nil
	}
	mode := strings.TrimSpace(evidence.Signer.Mode)
	if mode == "" || mode == "local_keypair" {
		return nil
	}
	return &RailMerchantAccountSignerConfig{
		Mode: mode,
		Key:  strings.TrimSpace(evidence.Signer.Key),
	}
}

func railMerchantAccountSettingsFromEvidence(raw []byte) map[string]any {
	var evidence struct {
		Settings map[string]any `json:"settings"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &evidence) != nil || len(evidence.Settings) == 0 {
		return nil
	}
	return evidence.Settings
}

// MarshalMerchantManifest renders a config manifest to YAML, the canonical dump output.
func MarshalMerchantManifest(m *BillingConfig) ([]byte, error) {
	return yaml.Marshal(m)
}

// railMerchantAccountSecrets lists the merchant's provider-account secret VALUES
// grouped by (rail, environment, account_id). It returns nothing unless
// includeValues is set: a redacted dump omits secret fields entirely rather than
// emitting a placeholder that a re-apply (--overwrite) could store as the real value.
func railMerchantAccountSecrets(ctx context.Context, secretStore merchants.MerchantSecretStore, mid merchant.ID, includeValues bool) (map[string]map[string]string, error) {
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
		gk := railMerchantAccountSecretGroupKey(rail, environment, accountID)
		if out[gk] == nil {
			out[gk] = map[string]string{}
		}
		out[gk][key] = secret.Value
	}
	return out, nil
}

func railMerchantAccountSecretGroupKey(rail, environment, accountID string) string {
	return strings.ToLower(rail) + "\x00" + strings.ToLower(environment) + "\x00" + accountID
}

func railMerchantAccountDumpKey(rail, environment, accountID string) string {
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
