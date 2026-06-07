package catalog

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// SupportedVersion is the only manifest schema version this tool accepts.
const SupportedVersion = 1

// intervalDays converts a recurring interval + count into billing-cycle days,
// matching cozy-art's mapping (month=30, year=365). OpenRails prices store a
// BillingCycleDays integer rather than an interval enum.
func intervalDays(interval string, count int) int {
	if count <= 0 {
		count = 1
	}
	switch interval {
	case "year":
		return 365 * count
	default:
		return 30 * count
	}
}

var invalidSlugChars = regexp.MustCompile(`[^a-z0-9_-]+`)

func normalizeSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = invalidSlugChars.ReplaceAllString(value, "-")
	return strings.Trim(value, "-_")
}

func normalizeCurrency(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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
	m.DefaultCurrency = normalizeCurrency(m.DefaultCurrency)
	if m.DefaultCurrency == "" {
		m.DefaultCurrency = "usd"
	}
	m.DefaultProviders = normalizeProviders(m.DefaultProviders)
	if len(m.TierGroups) == 0 {
		return errors.New("catalog manifest must define at least one tier group")
	}

	groupSlugs := map[string]struct{}{}
	// Product slugs are globally unique across the manifest: slug is the product's
	// identity in OpenRails (GetProductBySlug), so the same slug in two tier
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

		for pi := range group.Products {
			product := &group.Products[pi]
			if err := m.validateProduct(group.Slug, product, productSlugs); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manifest) validateProduct(groupSlug string, product *Product, productSlugs map[string]struct{}) error {
	product.Slug = normalizeSlug(product.Slug)
	if product.Slug == "" {
		return fmt.Errorf("tier group %q has a product without a slug", groupSlug)
	}
	if _, ok := productSlugs[product.Slug]; ok {
		return fmt.Errorf("duplicate product slug %q", product.Slug)
	}
	productSlugs[product.Slug] = struct{}{}

	if strings.TrimSpace(product.DisplayName) == "" {
		return fmt.Errorf("product %q display_name is required", product.Slug)
	}
	if product.TierRank <= 0 {
		return fmt.Errorf("product %q tier_rank must be positive", product.Slug)
	}
	var err error
	if product.Status, err = normalizeStatus(product.Status); err != nil {
		return fmt.Errorf("product %q: %w", product.Slug, err)
	}
	product.Providers = normalizeProviders(product.Providers)
	if len(product.Prices) == 0 {
		return fmt.Errorf("product %q must define prices", product.Slug)
	}

	// A price's identity is its financial substance. Dedup declared prices
	// within a product by (currency, unit_amount, interval, interval_count) —
	// there is no slug to dedup on.
	priceTerms := map[string]struct{}{}
	for pri := range product.Prices {
		price := &product.Prices[pri]
		if err := m.validatePrice(*product, price, pri); err != nil {
			return err
		}
		key := priceTermsKey(*price)
		if _, ok := priceTerms[key]; ok {
			return fmt.Errorf("product %q declares duplicate price terms %s", product.Slug, PriceLabel(product.Slug, *price))
		}
		priceTerms[key] = struct{}{}
	}
	return nil
}

func (m *Manifest) validatePrice(product Product, price *Price, idx int) error {
	price.Currency = normalizeCurrency(firstNonEmpty(price.Currency, m.DefaultCurrency))
	if price.Currency == "" {
		return fmt.Errorf("product %q price #%d currency is required", product.Slug, idx+1)
	}
	if price.UnitAmount <= 0 {
		return fmt.Errorf("product %q price #%d unit_amount must be positive", product.Slug, idx+1)
	}
	price.Interval = normalizeSlug(price.Interval)
	if price.Interval == "" {
		price.Interval = "month"
	}
	if price.Interval != "month" && price.Interval != "year" {
		return fmt.Errorf("product %q price #%d interval must be month or year", product.Slug, idx+1)
	}
	if price.IntervalCount <= 0 {
		price.IntervalCount = 1
	}
	var err error
	if price.Status, err = normalizeStatus(price.Status); err != nil {
		return fmt.Errorf("product %q price %s: %w", product.Slug, PriceLabel(product.Slug, *price), err)
	}
	// Legacy imports are archived by definition; reconcile the shorthand.
	if price.LegacyImport && price.Status == "" {
		price.Status = StatusArchived
	}

	if price.Providers != nil {
		price.Providers = normalizeProviders(price.Providers)
	}

	// Fold the cozy-art stripe_price_id shorthand into provider_links.stripe.
	if sid := strings.TrimSpace(price.StripePriceID); sid != "" {
		if price.ProviderLinks == nil {
			price.ProviderLinks = map[string]map[string]string{}
		}
		if price.ProviderLinks["stripe"] == nil {
			price.ProviderLinks["stripe"] = map[string]string{}
		}
		if _, ok := price.ProviderLinks["stripe"]["price_id"]; !ok {
			price.ProviderLinks["stripe"]["price_id"] = sid
		}
	}

	// Per-provider eligibility (shape-only; no chain calls). Solana settles
	// on-chain in $1-pegged stablecoins on a recurring schedule, so a Solana
	// price must be recurring (an interval is always present here) AND priced in
	// a stablecoin currency.
	for _, provider := range m.providersFor(product, *price) {
		if provider == "solana" {
			if _, ok := stablecoinCurrencies[price.Currency]; !ok {
				return fmt.Errorf("product %q price %s: solana requires a stablecoin currency (usd/usdc/usdg), got %q",
					product.Slug, PriceLabel(product.Slug, *price), price.Currency)
			}
		}
	}
	return nil
}

// priceTermsKey is the financial-identity key for a declared price.
func priceTermsKey(p Price) string {
	return fmt.Sprintf("%s|%d|%s|%d", p.Currency, p.UnitAmount, p.Interval, p.IntervalCount)
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
