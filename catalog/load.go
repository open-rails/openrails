package catalog

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/open-rails/openrails/pkg/pricing"
)

// SupportedVersion is the only manifest schema version this tool accepts.
const SupportedVersion = 1

// normalizeDuration parses a #622 access-window duration. An empty value or
// "indefinite" returns nil (durable/perpetual). A finite value must be a whole
// number of hours, minimum 1h (the storage unit), so windows can be sub-day
// (e.g. 12h). Returns the window length in HOURS, or nil for indefinite.
func normalizeDuration(value string) (*int, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "indefinite" {
		return nil, nil
	}
	d, err := ParseDurationSpec(value)
	if err != nil {
		return nil, err
	}
	if d < time.Hour || d%time.Hour != 0 {
		return nil, fmt.Errorf("duration %q must be whole hours (e.g. 12h, 30d) or 'indefinite'", value)
	}
	hours := int(d / time.Hour)
	return &hours, nil
}

func formatDurationHours(hours int) string {
	if hours%24 == 0 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dh", hours)
}

func normalizeSlug(value string) string {
	return pricing.NormalizeKey(value)
}

// normalizeCurrency canonicalises a currency code to UPPER case (CUR-6).
// Uppercase is the internal canonical form and what the DB CHECK accepts;
// lowercase belongs only on rail wires that demand it (Stripe).
func normalizeCurrency(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

// stablecoinCurrencies are the currencies eligible for the Solana provider.
// Solana settles on-chain in stablecoins pegged $1; a recurring fiat price is
// only Solana-eligible if its currency is one of these.
var stablecoinCurrencies = map[string]struct{}{
	"USD":  {}, // priced in USD, settled in a $1-pegged stablecoin
	"USDC": {},
	"USDG": {},
}

// Load reads, parses and validates a manifest from disk. Validation normalizes
// slugs/currencies/intervals in place and rejects structurally invalid
// manifests (bad version, duplicate slugs, duplicate prices by financial terms,
// provider-eligibility violations). It never touches the database or any chain.
func Load(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is caller-supplied (CLI/operator invocation), same trust boundary as os.ReadFile itself
	if err != nil {
		return nil, fmt.Errorf("read catalog manifest: %w", err)
	}
	return Parse(raw)
}

// retiredManifestKeys maps a key this manifest schema no longer has onto the
// rewrite an operator must apply. The parser is strict (DisallowUnknownField),
// so a retired key already fails; this only turns "unknown field" into the
// instruction. There are no sentinel struct fields — one mechanism, one shape.
var retiredManifestKeys = []struct {
	key  string
	hint string
}{
	{"metered", `the metered: price sugar was removed (or#893/#707) — declare a rate card instead:
    rate_cards:
      - meter: <meter-key>
        payment_term: in_arrears
        price:
          model: per_unit
          currency: <CUR>
          per_unit: {unit_amount: <rate micros>, divide_by: <per_units, × per-seconds for a former gauge>}
  A pure-usage price (unit_amount 0) becomes the rate card alone; a base fee keeps its price row.`},
	{"kind", `meter kind: counter|gauge was removed (or#893/#707) — declare aggregation instead:
    meters:
      - key: <meter-key>
        aggregation: sum      # a former counter that counted the event itself is aggregation: count
        value_property: <numeric property>`},
	{"providers", `providers: was renamed to psps:`},
	{"provider_links", `provider_links: was renamed to psp_links:`},
}

// Parse parses and validates a catalog manifest from YAML bytes.
func Parse(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.UnmarshalWithOptions(raw, &m, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parse catalog manifest: %w", annotateRetiredManifestKey(err))
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// annotateRetiredManifestKey appends the rewrite instruction when a strict-decode
// failure names a key this schema deliberately dropped.
func annotateRetiredManifestKey(err error) error {
	msg := err.Error()
	for _, retired := range retiredManifestKeys {
		if strings.Contains(msg, `unknown field "`+retired.key+`"`) {
			return fmt.Errorf("%w\n\n%s", err, retired.hint)
		}
	}
	return err
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
	if err := m.validateMeters(); err != nil {
		return err
	}

	groupKeys := map[string]struct{}{}
	// Product keys are globally unique across the manifest, so the same key in
	// two tier groups would collide on apply.
	productKeys := map[string]struct{}{}
	for gi := range m.TierGroups {
		group := &m.TierGroups[gi]
		group.Key = normalizeSlug(group.Key)
		if group.Key == "" {
			return errors.New("tier group key is required")
		}
		if _, ok := groupKeys[group.Key]; ok {
			return fmt.Errorf("duplicate tier group key %q", group.Key)
		}
		groupKeys[group.Key] = struct{}{}
		if len(group.Products) == 0 {
			return fmt.Errorf("tier group %q must define products", group.Key)
		}

		requireTierRank := len(group.Products) > 1 && group.Key != "default" && !strings.HasPrefix(group.Key, "usage-")
		for pi := range group.Products {
			product := &group.Products[pi]
			if err := m.validateProduct(group.Key, product, productKeys, requireTierRank); err != nil {
				return err
			}
		}
	}
	if err := m.validateRateCardModel(); err != nil {
		return err
	}
	return nil
}

func (m *Manifest) validateProduct(groupKey string, product *Product, productKeys map[string]struct{}, requireTierRank bool) error {
	product.Key = normalizeSlug(product.Key)
	if product.Key == "" {
		return fmt.Errorf("tier group %q has a product without a key", groupKey)
	}
	if _, ok := productKeys[product.Key]; ok {
		return fmt.Errorf("duplicate product key %q", product.Key)
	}
	productKeys[product.Key] = struct{}{}

	if strings.TrimSpace(product.DisplayName) == "" {
		return fmt.Errorf("product %q display_name is required", product.Key)
	}
	if requireTierRank && product.TierRank == nil {
		return fmt.Errorf("product %q tier_rank is required when tier group %q has multiple products", product.Key, groupKey)
	}

	// A price's identity is its financial substance — exactly the DB's
	// unique_prices_product_amount_window key. There is no price slug to dedup
	// on, and providers are NOT part of identity (the constraint forbids two
	// prices that differ only by provider).
	priceTerms := map[string]struct{}{}
	for pri := range product.Prices {
		price := &product.Prices[pri]
		if err := m.validatePrice(*product, price, pri); err != nil {
			return err
		}
		key := priceTermsKey(*price)
		if _, ok := priceTerms[key]; ok {
			return fmt.Errorf("product %q declares duplicate price terms %s", product.Key, fmt.Sprintf("#%d", pri+1))
		}
		priceTerms[key] = struct{}{}
	}
	if err := validateProductCapabilities(product); err != nil {
		return err
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
			// A usage-metered product isn't a tier-exclusive subscription (#642):
			// give it its own singleton group keyed by product key so it needs no
			// tier_group and never shares tier exclusivity with a sibling resource.
			// planProduct persists its tier_group as NULL.
			if len(p.RateCards) > 0 {
				group = "usage:" + normalizeSlug(p.Key)
			} else {
				group = "default"
			}
		}
		idx, ok := groups[group]
		if !ok {
			idx = len(m.TierGroups)
			groups[group] = idx
			m.TierGroups = append(m.TierGroups, TierGroup{Key: group, DisplayName: group})
		}
		m.TierGroups[idx].Products = append(m.TierGroups[idx].Products, p)
	}
	return nil
}

func validateProductCapabilities(product *Product) error {
	if len(product.RateCards) > 0 && (strings.TrimSpace(product.TierGroup) != "" || product.TierRank != nil) {
		return fmt.Errorf("product %q usage/rate-card product must not set tier_group/tier_rank", product.Key)
	}
	if strings.TrimSpace(product.TierGroup) != "" {
		hasRecurring := false
		for _, price := range product.Prices {
			if price.AutoRenew {
				hasRecurring = true
				break
			}
		}
		if !hasRecurring {
			return fmt.Errorf("product %q membership tier requires a recurring price", product.Key)
		}
	}
	return nil
}

func (m *Manifest) validatePrice(product Product, price *Price, idx int) error {
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
	if price.UnitAmount == 0 {
		return fmt.Errorf("product %q price #%d unit_amount must be positive", product.Key, idx+1)
	}
	durHours, err := normalizeDuration(price.Duration)
	if err != nil {
		return fmt.Errorf("product %q price #%d duration: %w", product.Key, idx+1, err)
	}
	if durHours == nil {
		price.Duration = "indefinite"
	} else {
		price.Duration = formatDurationHours(*durHours)
	}
	// Nothing to renew without a finite window.
	if price.AutoRenew && durHours == nil {
		return fmt.Errorf("product %q price #%d: auto_renew requires a finite duration (not indefinite)", product.Key, idx+1)
	}
	if price.Trial != nil {
		trialHours, err := normalizeDuration(price.Trial.Duration)
		if err != nil {
			return fmt.Errorf("product %q price #%d trial.duration: %w", product.Key, idx+1, err)
		}
		if trialHours == nil {
			return fmt.Errorf("product %q price #%d trial.duration must be a finite duration", product.Key, idx+1)
		}
		if !price.AutoRenew {
			return fmt.Errorf("product %q price #%d trial requires auto_renew (a first phase then recurring terms)", product.Key, idx+1)
		}
		if price.Trial.UnitAmount < 0 {
			return fmt.Errorf("product %q price #%d trial.unit_amount must be >= 0 (0 = free trial)", product.Key, idx+1)
		}
		price.Trial.Duration = formatDurationHours(*trialHours)
	}
	if price.PSPs != nil {
		price.PSPs = normalizePSPs(price.PSPs)
	}
	if len(price.PSPLinks) > 0 {
		declared := map[string]struct{}{}
		for _, provider := range price.PSPs {
			declared[provider] = struct{}{}
		}
		for provider := range price.PSPLinks {
			key := strings.ToLower(strings.TrimSpace(provider))
			if _, ok := declared[key]; !ok {
				return fmt.Errorf("product %q price %s: psp_links.%s requires psps to include %q", product.Key, fmt.Sprintf("#%d", idx+1), provider, key)
			}
		}
	}

	// Per-provider eligibility (shape-only; no chain calls). Solana settles
	// on-chain in $1-pegged stablecoins, so a Solana price must be priced in a
	// stablecoin currency (one-off finite windows and recurring are both allowed).
	for _, provider := range price.PSPs {
		if provider == "solana" {
			if _, ok := stablecoinCurrencies[price.Currency]; !ok {
				return fmt.Errorf("product %q price %s: solana requires a stablecoin currency (USD/USDC/USDG), got %q",
					product.Key, fmt.Sprintf("#%d", idx+1), price.Currency)
			}
		}
	}
	return nil
}

func (m *Manifest) validateMeters() error {
	seen := map[string]struct{}{}
	for i := range m.Meters {
		meter := &m.Meters[i]
		validated := pricing.Meter{
			Key:           meter.Key,
			EventType:     meter.EventType,
			ValueProperty: meter.ValueProperty,
			Aggregation:   meter.Aggregation,
			Unit:          meter.Unit,
			GroupBy:       meter.GroupBy,
		}
		if err := pricing.ValidateMeter(fmt.Sprintf("meter #%d", i+1), &validated); err != nil {
			return err
		}
		meter.Key = validated.Key
		meter.EventType = validated.EventType
		meter.ValueProperty = validated.ValueProperty
		meter.Aggregation = validated.Aggregation
		meter.Unit = validated.Unit
		meter.GroupBy = validated.GroupBy
		if _, ok := seen[meter.Key]; ok {
			return fmt.Errorf("duplicate meter key %q", meter.Key)
		}
		seen[meter.Key] = struct{}{}
	}
	return nil
}

func validLedgerCurrency(value string) bool {
	value = normalizeCurrency(value)
	if value == "CUSTOM" {
		return true
	}
	if _, ok := stablecoinCurrencies[value]; ok {
		return true
	}
	if len(value) != 3 {
		return false
	}
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func validPriceCurrency(value string) bool {
	value = normalizeCurrency(value)
	return value != "CUSTOM" && validLedgerCurrency(value)
}

// priceTermsKey is the manifest identity key for a declared price — the same
// financial substance the DB's unique_prices_product_amount_window key enforces.
// PSPs are excluded so two declarations that differ only by PSP are
// rejected here with a clear message instead of colliding on the unique key at
// apply time.
func priceTermsKey(p Price) string {
	trial := ""
	if p.Trial != nil {
		trial = fmt.Sprintf("|trial:%d:%s", p.Trial.UnitAmount, p.Trial.Duration)
	}
	return fmt.Sprintf("%s|%d|%s|renew:%t%s", p.Currency, p.UnitAmount, p.Duration, p.AutoRenew, trial)
}

func normalizePSPs(in []string) []string {
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
