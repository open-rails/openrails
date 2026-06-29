package catalog

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// SupportedVersion is the only manifest schema version this tool accepts.
const SupportedVersion = 1

var invalidSlugChars = regexp.MustCompile(`[^a-z0-9_-]+`)

// normalizeDuration parses a #622 access-window duration. An empty value or
// "indefinite" returns nil (durable/perpetual). A finite value must be a whole
// number of days (the storage unit). Returns the window length in days, or nil
// for indefinite.
func normalizeDuration(value string) (*int, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "indefinite" {
		return nil, nil
	}
	d, err := ParseDurationSpec(value)
	if err != nil {
		return nil, err
	}
	if d < 24*time.Hour || d%(24*time.Hour) != 0 {
		return nil, fmt.Errorf("duration %q must be whole days (e.g. 30d) or 'indefinite'", value)
	}
	days := int(d / (24 * time.Hour))
	return &days, nil
}

func normalizeSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = invalidSlugChars.ReplaceAllString(value, "-")
	return strings.Trim(value, "-_")
}

func normalizeCurrency(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// stablecoinCurrencies are the currencies eligible for the Solana provider.
// Solana settles on-chain in stablecoins pegged $1; a recurring fiat price is
// only Solana-eligible if its currency is one of these.
var stablecoinCurrencies = map[string]struct{}{
	"usd":  {}, // priced in USD, settled in a $1-pegged stablecoin
	"usdc": {},
	"usdg": {},
}

// Load reads, parses and validates a manifest from disk. Validation normalizes
// slugs/currencies/intervals in place and rejects structurally invalid
// manifests (bad version, duplicate slugs, duplicate prices by financial terms,
// provider-eligibility violations). It never touches the database or any chain.
func Load(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog manifest: %w", err)
	}
	return Parse(raw)
}

// Parse parses and validates a catalog manifest from YAML bytes.
func Parse(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.UnmarshalWithOptions(raw, &m, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parse catalog manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate normalizes and validates a manifest in place.
func (m *Manifest) Validate() error {
	if m == nil {
		return errors.New("catalog manifest is required")
	}
	return m.validate()
}

func (m *Manifest) validate() error {
	if m.Version != SupportedVersion {
		return fmt.Errorf("unsupported catalog manifest version %d (want %d)", m.Version, SupportedVersion)
	}
	if err := m.normalizeProducts(); err != nil {
		return err
	}
	if len(m.TierGroups) == 0 {
		return errors.New("catalog manifest must define products")
	}
	meterKinds, err := m.validateMeters()
	if err != nil {
		return err
	}
	usageLimitKeys, err := m.validateUsageLimits()
	if err != nil {
		return err
	}

	groupSlugs := map[string]struct{}{}
	// Product keys are globally unique across the manifest. OpenRails still
	// stores this value in products.slug, so the same key in two tier
	// groups would collide on apply.
	productSlugs := map[string]struct{}{}

	for gi := range m.TierGroups {
		group := &m.TierGroups[gi]
		group.Slug = normalizeSlug(group.Slug)
		if group.Slug == "" {
			return errors.New("tier group slug is required")
		}
		if _, ok := groupSlugs[group.Slug]; ok {
			return fmt.Errorf("duplicate tier group slug %q", group.Slug)
		}
		groupSlugs[group.Slug] = struct{}{}
		if len(group.Products) == 0 {
			return fmt.Errorf("tier group %q must define products", group.Slug)
		}

		requireTierRank := len(group.Products) > 1
		for pi := range group.Products {
			product := &group.Products[pi]
			if err := m.validateProduct(group.Slug, product, productSlugs, requireTierRank, meterKinds); err != nil {
				return err
			}
		}
	}
	if err := m.validateProductBenefitRefs(productSlugs, usageLimitKeys); err != nil {
		return err
	}
	return nil
}

func (m *Manifest) validateProduct(groupSlug string, product *Product, productSlugs map[string]struct{}, requireTierRank bool, meterKinds map[string]string) error {
	product.Key = normalizeSlug(product.Key)
	if product.Key == "" {
		return fmt.Errorf("tier group %q has a product without a key", groupSlug)
	}
	if _, ok := productSlugs[product.Key]; ok {
		return fmt.Errorf("duplicate product key %q", product.Key)
	}
	productSlugs[product.Key] = struct{}{}

	if strings.TrimSpace(product.DisplayName) == "" {
		return fmt.Errorf("product %q display_name is required", product.Key)
	}
	if requireTierRank && product.TierRank == nil {
		return fmt.Errorf("product %q tier_rank is required when tier group %q has multiple products", product.Key, groupSlug)
	}
	if err := validateCredits(product.Key, product.Credits); err != nil {
		return err
	}

	// A price's identity is its financial substance plus explicit provider set.
	// There is no price slug to dedup on.
	priceTerms := map[string]struct{}{}
	for pri := range product.Prices {
		price := &product.Prices[pri]
		if err := m.validatePrice(*product, price, pri, meterKinds); err != nil {
			return err
		}
		key := priceTermsKey(*price)
		if _, ok := priceTerms[key]; ok {
			return fmt.Errorf("product %q declares duplicate price terms %s", product.Key, PriceLabel(product.Key, *price))
		}
		priceTerms[key] = struct{}{}
	}
	return nil
}

func (m *Manifest) normalizeProducts() error {
	if len(m.Products) == 0 && len(m.TierGroups) > 0 {
		return errors.New("catalog manifest must define products, not tier_groups")
	}
	m.TierGroups = nil
	if len(m.Products) == 0 {
		return nil
	}
	groups := map[string]int{}
	for _, p := range m.Products {
		group := normalizeSlug(p.TierGroup)
		if group == "" {
			group = "default"
		}
		idx, ok := groups[group]
		if !ok {
			idx = len(m.TierGroups)
			groups[group] = idx
			m.TierGroups = append(m.TierGroups, TierGroup{Slug: group, DisplayName: group})
		}
		m.TierGroups[idx].Products = append(m.TierGroups[idx].Products, p)
	}
	return nil
}

func validateCredits(productSlug string, credits Credits) error {
	for key, credit := range credits {
		if normalizeSlug(key) == "" {
			return fmt.Errorf("product %q has credit with empty key", productSlug)
		}
		if strings.TrimSpace(credit.Unit) == "" && strings.TrimSpace(credit.Currency) != "" {
			credit.Unit = credit.Currency
			credits[key] = credit
		}
		if unit := strings.ToLower(strings.TrimSpace(credit.Unit)); unit != "" && !validCreditUnit(unit) {
			return fmt.Errorf("product %q credit %q has invalid unit %q", productSlug, key, credit.Unit)
		}
		if credit.Amount <= 0 {
			return fmt.Errorf("product %q credit %q amount must be positive", productSlug, key)
		}
		if strings.TrimSpace(credit.Expires) != "" {
			if _, err := ParseDurationSpec(credit.Expires); err != nil {
				return fmt.Errorf("product %q credit %q expires: %w", productSlug, key, err)
			}
		}
		switch strings.TrimSpace(credit.Cadence) {
		case "", "once", "per_renewal":
		default:
			return fmt.Errorf("product %q credit %q cadence must be once or per_renewal", productSlug, key)
		}
	}
	return nil
}

func (m *Manifest) validatePrice(product Product, price *Price, idx int, meterKinds map[string]string) error {
	price.Currency = normalizeCurrency(price.Currency)
	if price.Currency == "" {
		return fmt.Errorf("product %q price #%d currency is required", product.Key, idx+1)
	}
	if !validPriceCurrency(price.Currency) {
		return fmt.Errorf("product %q price #%d currency must be an ISO money currency", product.Key, idx+1)
	}
	if price.UnitAmount < 0 {
		return fmt.Errorf("product %q price #%d unit_amount must be non-negative", product.Key, idx+1)
	}
	if price.UnitAmount == 0 && price.Metered == nil {
		return fmt.Errorf("product %q price #%d unit_amount must be positive", product.Key, idx+1)
	}
	if err := m.validateMeteredPrice(product, price, idx, meterKinds); err != nil {
		return err
	}
	durDays, err := normalizeDuration(price.Duration)
	if err != nil {
		return fmt.Errorf("product %q price #%d duration: %w", product.Key, idx+1, err)
	}
	if durDays == nil {
		price.Duration = "indefinite"
	} else {
		price.Duration = fmt.Sprintf("%dd", *durDays)
	}
	// Nothing to renew without a finite window.
	if price.AutoRenew && durDays == nil {
		return fmt.Errorf("product %q price #%d: auto_renew requires a finite duration (not indefinite)", product.Key, idx+1)
	}
	if price.Trial != nil {
		trialDays, err := normalizeDuration(price.Trial.Duration)
		if err != nil {
			return fmt.Errorf("product %q price #%d trial.duration: %w", product.Key, idx+1, err)
		}
		if trialDays == nil {
			return fmt.Errorf("product %q price #%d trial.duration must be a finite duration", product.Key, idx+1)
		}
		if !price.AutoRenew {
			return fmt.Errorf("product %q price #%d trial requires auto_renew (a first phase then recurring terms)", product.Key, idx+1)
		}
		if price.Trial.UnitAmount < 0 {
			return fmt.Errorf("product %q price #%d trial.unit_amount must be >= 0 (0 = free trial)", product.Key, idx+1)
		}
		price.Trial.Duration = fmt.Sprintf("%dd", *trialDays)
	}
	if price.Providers != nil {
		price.Providers = normalizeProviders(price.Providers)
	}
	if len(price.ProviderLinks) > 0 {
		declared := map[string]struct{}{}
		for _, provider := range price.Providers {
			declared[provider] = struct{}{}
		}
		for provider := range price.ProviderLinks {
			key := strings.ToLower(strings.TrimSpace(provider))
			if _, ok := declared[key]; !ok {
				return fmt.Errorf("product %q price %s: provider_links.%s requires providers to include %q", product.Key, PriceLabel(product.Key, *price), provider, key)
			}
		}
	}

	// Per-provider eligibility (shape-only; no chain calls). Solana settles
	// on-chain in $1-pegged stablecoins, so a Solana price must be priced in a
	// stablecoin currency (one-off finite windows and recurring are both allowed).
	if price.Metered != nil && len(price.Providers) > 0 {
		return fmt.Errorf("product %q price %s: metered prices are OpenRails-native and must not declare external providers", product.Key, PriceLabel(product.Key, *price))
	}
	for _, provider := range price.Providers {
		if provider == "solana" {
			if _, ok := stablecoinCurrencies[price.Currency]; !ok {
				return fmt.Errorf("product %q price %s: solana requires a stablecoin currency (usd/usdc/usdg), got %q",
					product.Key, PriceLabel(product.Key, *price), price.Currency)
			}
		}
	}
	return nil
}

func (m *Manifest) validateMeters() (map[string]string, error) {
	out := map[string]string{}
	for i := range m.Meters {
		meter := &m.Meters[i]
		meter.Key = normalizeSlug(meter.Key)
		if meter.Key == "" {
			return nil, fmt.Errorf("meter #%d key is required", i+1)
		}
		if _, ok := out[meter.Key]; ok {
			return nil, fmt.Errorf("duplicate meter key %q", meter.Key)
		}
		meter.Kind = strings.ToLower(strings.TrimSpace(meter.Kind))
		switch meter.Kind {
		case "counter", "gauge":
		default:
			return nil, fmt.Errorf("meter %q kind must be counter or gauge", meter.Key)
		}
		out[meter.Key] = meter.Kind
	}
	return out, nil
}

func (m *Manifest) validateMeteredPrice(product Product, price *Price, idx int, meterKinds map[string]string) error {
	if price.Metered == nil {
		return nil
	}
	mp := price.Metered
	mp.Meter = normalizeSlug(mp.Meter)
	kind, ok := meterKinds[mp.Meter]
	if !ok {
		return fmt.Errorf("product %q price #%d references unknown meter %q", product.Key, idx+1, mp.Meter)
	}
	if mp.Rate <= 0 {
		return fmt.Errorf("product %q price #%d metered rate must be positive", product.Key, idx+1)
	}
	if mp.PerUnits == 0 {
		mp.PerUnits = 1
	}
	if mp.PerUnits < 1 {
		return fmt.Errorf("product %q price #%d metered per_units must be >= 1", product.Key, idx+1)
	}
	mp.Per = strings.ToLower(strings.TrimSpace(mp.Per))
	switch kind {
	case "gauge":
		if mp.Per == "" {
			return fmt.Errorf("product %q price #%d gauge meter %q requires per", product.Key, idx+1, mp.Meter)
		}
		if _, err := ParseDurationSpec(mp.Per); err != nil {
			return fmt.Errorf("product %q price #%d metered per: %w", product.Key, idx+1, err)
		}
	case "counter":
		if mp.Per != "" {
			return fmt.Errorf("product %q price #%d counter meter %q must not set per", product.Key, idx+1, mp.Meter)
		}
	}
	return nil
}

func (m *Manifest) validateUsageLimits() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for i := range m.UsageLimits {
		limit := &m.UsageLimits[i]
		limit.Key = normalizeSlug(limit.Key)
		limit.Measure = normalizeSlug(limit.Measure)
		if limit.Key == "" {
			return nil, fmt.Errorf("usage_limit #%d key is required", i+1)
		}
		if _, ok := out[limit.Key]; ok {
			return nil, fmt.Errorf("duplicate usage_limit key %q", limit.Key)
		}
		if limit.Measure == "" {
			return nil, fmt.Errorf("usage_limit %q measure is required", limit.Key)
		}
		if len(limit.Windows) == 0 {
			return nil, fmt.Errorf("usage_limit %q must define windows", limit.Key)
		}
		seenWindows := map[string]struct{}{}
		for wi := range limit.Windows {
			w := &limit.Windows[wi]
			w.Window = strings.ToLower(strings.TrimSpace(w.Window))
			if _, err := ParseDurationSpec(w.Window); err != nil {
				return nil, fmt.Errorf("usage_limit %q window: %w", limit.Key, err)
			}
			if w.Amount <= 0 {
				return nil, fmt.Errorf("usage_limit %q window %q amount must be positive", limit.Key, w.Window)
			}
			if _, ok := seenWindows[w.Window]; ok {
				return nil, fmt.Errorf("usage_limit %q has duplicate window %q", limit.Key, w.Window)
			}
			seenWindows[w.Window] = struct{}{}
		}
		out[limit.Key] = struct{}{}
	}
	return out, nil
}

func (m *Manifest) validateProductBenefitRefs(productSlugs, usageLimitKeys map[string]struct{}) error {
	for gi := range m.TierGroups {
		for pi := range m.TierGroups[gi].Products {
			product := &m.TierGroups[gi].Products[pi]
			for i, key := range product.UsageLimits {
				key = normalizeSlug(key)
				product.UsageLimits[i] = key
				if _, ok := usageLimitKeys[key]; !ok {
					return fmt.Errorf("product %q references unknown usage_limit %q", product.Key, key)
				}
			}
			for i, key := range product.Includes {
				key = normalizeSlug(key)
				product.Includes[i] = key
				if _, ok := productSlugs[key]; !ok {
					return fmt.Errorf("product %q includes unknown product %q", product.Key, key)
				}
				if key == product.Key {
					return fmt.Errorf("product %q cannot include itself", product.Key)
				}
			}
		}
	}
	return nil
}

func validLedgerCurrency(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "custom" {
		return true
	}
	if _, ok := stablecoinCurrencies[value]; ok {
		return true
	}
	if len(value) != 3 {
		return false
	}
	for _, r := range value {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func validCreditUnit(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if left, right, ok := strings.Cut(value, "/"); ok {
		return normalizeSlug(left) == left && normalizeSlug(right) == right
	}
	return validLedgerCurrency(value) || normalizeSlug(value) == value
}

func validPriceCurrency(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value != "custom" && validLedgerCurrency(value)
}

// priceTermsKey is the manifest identity key for a declared price.
func priceTermsKey(p Price) string {
	metered := ""
	if p.Metered != nil {
		metered = fmt.Sprintf("|metered:%s:%d:%d:%s", p.Metered.Meter, p.Metered.Rate, p.Metered.PerUnits, p.Metered.Per)
	}
	return fmt.Sprintf("%s|%d|%s|renew:%t|providers:%s%s", p.Currency, p.UnitAmount, p.Duration, p.AutoRenew, providerSetKey(p.Providers), metered)
}

func providerSetKey(providers []string) string {
	if len(providers) == 0 {
		return ""
	}
	out := append([]string(nil), providers...)
	sort.Strings(out)
	return strings.Join(out, ",")
}

func normalizeProviders(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, p := range in {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
