package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/pkg/merchant"
)

type DumpMerchantConfigOptions struct {
	IncludeSecrets bool
}

// DumpMerchantConfig reads a merchant's OpenRails-owned configuration (identity,
// profile, invoice/collection policy, delegated-invoker windows, and PSPs)
// and returns it in the push-merchant-config YAML shape (#646/#653).
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

	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return nil, err
	}
	directory.WithNameAuthority(controlplane.MerchantNameAuthority(cp.Core()))
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
		for _, rule := range conf.CheckoutRouting {
			mt.CheckoutRouting = append(mt.CheckoutRouting, CheckoutRoutingRuleConfig{
				Match: CheckoutRoutingMatchConfig{
					Currency: rule.Match.Currency,
					Product:  rule.Match.Product,
					Price:    rule.Match.Price,
					Mode:     rule.Match.Mode,
					Country:  rule.Match.Country,
				},
				Prefer: rule.Prefer,
			})
		}
	}

	// custodians (or#880) — dumped BEFORE the PSPs that reference them, and
	// keyed by row id so each PSP can emit its `custodian:` reference. Omitting
	// them would round-trip a custody arrangement into an unarmed one.
	var declaredCustodians []gen.OpenrailsCustodian
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
		entry := CustodianAccountConfig{AccountID: c.AccountID, Archived: c.Archived}
		var settings map[string]any
		if len(c.Settings) > 0 && json.Unmarshal(c.Settings, &settings) == nil && len(settings) > 0 {
			entry.Settings = settings
		}
		if values := custodianSecrets[custodianSecretGroupKey(c.Kind, c.Environment, c.AccountID)]; len(values) > 0 {
			entry.Secrets = values
		}
		mt.Custodians[key] = CustodianConfig{c.Kind: entry}
	}

	// or#897 billing policies + bindings. Dumped so a merchant configured through
	// the mode-2 API round-trips back into a mode-1 manifest unchanged.
	policyStore := admission.NewBillingPolicyStore(database)
	storedPolicies, err := policyStore.ListPolicies(mctx)
	if err != nil {
		return nil, fmt.Errorf("load billing policies: %w", err)
	}
	for name, body := range storedPolicies {
		if mt.BillingPolicies == nil {
			mt.BillingPolicies = map[string]BillingPolicyConfig{}
		}
		entry := BillingPolicyConfig{
			Kind:                   string(body.Kind),
			OutstandingCap:         body.OutstandingCapAmount,
			SpendWindows:           dumpBudgetWindows(body.SpendWindows),
			AccrualRateCapPerHour:  body.AccrualRateCapPerHour,
			BadSpendWindows:        dumpBudgetWindows(body.BadSpendWindows),
			CollectionThreshold:    body.CollectionThresholdAmount,
			DelinquencyGraceDays:   body.DelinquencyGraceDays,
			DelinquencyAmountFloor: body.DelinquencyAmountFloor,
			PolicyCurrency:         body.PolicyCurrency,
		}
		if body.AccrualRateWindowSeconds > 0 {
			entry.AccrualRateWindow = formatWindowSeconds(body.AccrualRateWindowSeconds)
		}
		mt.BillingPolicies[name] = entry
	}
	storedBindings, err := policyStore.ListDeclarativeBindings(mctx)
	if err != nil {
		return nil, fmt.Errorf("load billing policy bindings: %w", err)
	}
	for _, b := range storedBindings {
		mt.BillingPolicyBindings = append(mt.BillingPolicyBindings, BillingPolicyBindingConfig{
			Policy: b.PolicyName, Tier: b.Tier,
		})
	}

	// PSPs (identity + lifecycle + secret references).
	var accounts []gen.OpenrailsPsp
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var lerr error
		accounts, lerr = database.Gen(ctx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{
			MerchantID: mid.UUID(),
		})
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
		localKey := ""
		if a.Key != nil {
			localKey = strings.TrimSpace(*a.Key)
		}
		if localKey == "" {
			localKey = pspDumpKey(a.Rail, a.Environment, a.AccountID)
		}
		// #882: environment is derived from test_mode, so it is never emitted —
		// dumping it would round-trip into the removal error on apply.
		account := ProviderRailAccountConfig{
			AccountID: a.AccountID,
			Archived:  a.Archived,
		}
		if a.CustodianID != nil {
			account.Custodian = custodianKeyByID[*a.CustodianID]
		}
		if signer := pspSignerFromEvidence(a.Evidence); signer != nil {
			account.Signer = signer
		}
		if settings := pspSettingsFromEvidence(a.Evidence); len(settings) > 0 {
			account.Settings = settings
		}
		key := pspSecretGroupKey(a.Rail, a.Environment, a.AccountID)
		if values := secretValuesByAccount[key]; len(values) > 0 {
			account.Secrets = values
		}
		mt.PSPs[localKey] = PSPConfig{a.Rail: account}
	}

	return &BillingConfig{Version: BootstrapManifestVersion, Merchants: map[string]MerchantConfig{slug: mt}}, nil
}

func pspSignerFromEvidence(raw []byte) *PSPSignerConfig {
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
	return &PSPSignerConfig{
		Mode: mode,
		Key:  strings.TrimSpace(evidence.Signer.Key),
	}
}

func pspSettingsFromEvidence(raw []byte) map[string]any {
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

// pspSecrets lists the merchant's PSP secret VALUES
// grouped by (rail, environment, account_id). It returns nothing unless
// includeValues is set: a redacted dump omits secret fields entirely rather than
// emitting a placeholder that a re-apply (--overwrite) could store as the real value.
func pspSecrets(ctx context.Context, secretStore merchants.MerchantSecretStore, mid merchant.ID, includeValues bool) (map[string]map[string]string, error) {
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
func custodianSecretValues(ctx context.Context, secretStore merchants.MerchantSecretStore, mid merchant.ID, includeValues bool) (map[string]map[string]string, error) {
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

// dumpBudgetWindows projects stored windows (seconds) back onto the manifest
// shape (Go duration strings).
func dumpBudgetWindows(in []models.BudgetWindowPolicy) []BudgetWindowConfig {
	if len(in) == 0 {
		return nil
	}
	out := make([]BudgetWindowConfig, 0, len(in))
	for _, w := range in {
		if w.WindowSeconds <= 0 {
			continue
		}
		out = append(out, BudgetWindowConfig{
			Key:      w.Key,
			Window:   formatWindowSeconds(w.WindowSeconds),
			Limit:    w.Limit,
			Currency: w.Currency,
		})
	}
	return out
}
