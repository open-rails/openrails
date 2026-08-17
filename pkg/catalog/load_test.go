package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

const goodManifest = `
version: 1

products:
  - key: initiate
    display_name: Novice
    tier_group: cozy
    tier_rank: 1
    entitlements: [tier:initiate]
    prices:
      - currency: usd
        unit_amount: 1200
        duration: 30d
        auto_renew: true
        psps: [stripe]
  - key: craftsman
    display_name: Craftsman
    tier_group: cozy
    tier_rank: 2
    entitlements: [tier:craftsman]
    prices:
      - currency: usd
        unit_amount: 2900
        duration: 30d
        auto_renew: true
        psps: [stripe]
      - currency: usd
        unit_amount: 1500
        duration: 30d
        auto_renew: true
        archived: true
        psps: [stripe]
        psp_links:
          stripe:
            price_id: price_legacy123
`

func TestLoad_Good(t *testing.T) {
	m, err := Load(writeManifest(t, goodManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Version != 1 {
		t.Fatalf("unexpected header: %+v", m)
	}
	if len(m.TierGroups) != 1 || len(m.TierGroups[0].Products) != 2 {
		t.Fatalf("unexpected structure: %+v", m.TierGroups)
	}
	craftsman := m.TierGroups[0].Products[1]
	if len(craftsman.Prices) != 2 {
		t.Fatalf("want 2 prices, got %d", len(craftsman.Prices))
	}
	// Duration defaults and normalization.
	if craftsman.Prices[0].Duration != "30d" {
		t.Fatalf("duration not normalized: %+v", craftsman.Prices[0])
	}
	// Historical prices are declared directly as archived.
	legacy := craftsman.Prices[1]
	if !legacy.Archived {
		t.Fatalf("historical price should be archived, got %v", legacy.Archived)
	}
	if got := legacy.PSPLinks["stripe"]["price_id"]; got != "price_legacy123" {
		t.Fatalf("psp_links.stripe.price_id not preserved: %q", got)
	}
	if got := craftsman.Prices[0].PSPs; len(got) != 1 || got[0] != "stripe" {
		t.Fatalf("psps not normalized: %v", got)
	}
}

func TestLoad_BadVersion(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 2\nproducts: []\n"))
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("want unsupported version error, got %v", err)
	}
}

func TestLoad_RejectsTierGroups(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 1\ntier_groups: []\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want tier_groups unknown-field error, got %v", err)
	}
}

func TestLoad_RejectsStatus(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 1\nproducts:\n  - key: p\n    display_name: P\n    status: archived\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want status unknown-field error, got %v", err)
	}
}

func TestLoad_RejectsDefaultProviders(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 1\ndefault_providers: [stripe]\nproducts: []\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want default_providers unknown-field error, got %v", err)
	}
}

func TestLoad_RejectsProductProviders(t *testing.T) {
	_, err := Load(writeManifest(t, "version: 1\nproducts:\n  - key: p\n    display_name: P\n    psps: [stripe]\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want product providers unknown-field error, got %v", err)
	}
}

func TestLoad_ProviderLinksRequirePriceProvider(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    prices:
      - currency: usd
        unit_amount: 1000
        duration: 30d
        psp_links:
          stripe:
            lookup_key: p-monthly
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "requires psps to include") {
		t.Fatalf("want provider_links/provider mismatch error, got %v", err)
	}
}

func TestLoad_RejectsRetiredProviderKeysForTypedPrice(t *testing.T) {
	body := `
version: 1
products:
  - key: topup
    display_name: Topup
    credits: [{key: credits}]
    prices:
      - currency: usd
        model: tiered
        providers: [stripe]
        tiered:
          mode: graduated
          tiers:
            - {unit_amount: 10_000}
credit_balances:
  - {key: credits, unit: credit}
`
	_, err := Load(writeManifest(t, body))
	if err == nil {
		t.Fatal("a retired providers: key must not load")
	}
	for _, want := range []string{`unknown field "providers"`, "providers: was renamed to psps:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must carry %q, got:\n%v", want, err)
		}
	}
}

// or#893 phase 7: provider_links: is the same one mechanism — strict decoding
// plus the rename, not a sentinel struct field.
func TestLoad_RejectsRetiredProviderLinksKey(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    prices:
      - currency: usd
        unit_amount: 1000
        duration: 30d
        psps: [stripe]
        provider_links:
          stripe:
            lookup_key: p-monthly
`
	_, err := Load(writeManifest(t, body))
	if err == nil {
		t.Fatal("a retired provider_links: key must not load")
	}
	for _, want := range []string{`unknown field "provider_links"`, "provider_links: was renamed to psp_links:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must carry %q, got:\n%v", want, err)
		}
	}
}

func TestLoad_RejectsNoncanonicalCreditGrantFields(t *testing.T) {
	tests := []struct {
		name      string
		field     string
		yamlValue string
		jsonValue string
	}{
		{name: "unit", field: "unit", yamlValue: "other-credit", jsonValue: `"other-credit"`},
		{name: "currency", field: "currency", yamlValue: "usd", jsonValue: `"usd"`},
		{name: "expiry hours", field: "expiry_hours", yamlValue: "24", jsonValue: "24"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `
version: 1
credit_balances:
  - {key: credits, unit: credit}
products:
  - key: product
    display_name: Product
    credits:
      - key: credits
        amount: 10
        ` + tt.field + `: ` + tt.yamlValue + `
`
			_, err := Parse([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("want noncanonical %s rejection, got %v", tt.field, err)
			}

			var grant CreditGrant
			err = json.Unmarshal([]byte(`{"key":"credits","`+tt.field+`":`+tt.jsonValue+`}`), &grant)
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("want noncanonical JSON %s rejection, got %v", tt.field, err)
			}
		})
	}
}

func TestValidate_CreditGrantPreservesInternalCurrency(t *testing.T) {
	amount := int64(10)
	m := &Manifest{
		Version: SupportedVersion,
		CreditBalances: []CreditBalance{
			{Key: "credits", Unit: "credit"},
		},
		Products: []Product{
			{
				Key:         "product",
				DisplayName: "Product",
				Credits: []CreditGrant{
					{Key: "credits", Currency: "usd", Amount: &amount},
				},
			},
		},
	}

	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	grant := m.TierGroups[0].Products[0].Credits[0]
	if grant.Unit != "CREDIT" {
		t.Fatalf("credit did not derive balance unit: %q", grant.Unit)
	}
	if grant.Currency != "usd" {
		t.Fatalf("validation discarded internal currency: %q", grant.Currency)
	}
}

func TestValidateRejectsTierGroupsOnly(t *testing.T) {
	m := &Manifest{
		Version:    SupportedVersion,
		TierGroups: []TierGroup{{Key: "old", Products: []Product{{Key: "p", DisplayName: "P"}}}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "not tier_groups") {
		t.Fatalf("want tier_groups-only rejection, got %v", err)
	}
}

func TestValidateProductsIsIdempotent(t *testing.T) {
	m, err := Load(writeManifest(t, goodManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("second Validate: %v", err)
	}
}

func TestLoad_DuplicatePriceByTerms(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 30d, auto_renew: true}
      - {currency: usd, unit_amount: 1000, duration: 30d, auto_renew: true}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "duplicate price terms") {
		t.Fatalf("want duplicate price terms error, got %v", err)
	}
}

// Two prices that differ only by provider share one unique_prices_product_amount_window
// key, so the DB can hold at most one. The loader must reject the manifest up
// front rather than let it collide on the unique key at apply time.
func TestLoad_RejectsSameTermsWithDifferentProviders(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    prices:
      - {currency: usd, unit_amount: 23000000, duration: 30d, psps: [mobius, ccbill, solana]}
      - {currency: usd, unit_amount: 23000000, duration: 30d, psps: [solana], archived: true}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "duplicate price terms") {
		t.Fatalf("want duplicate price terms error, got %v", err)
	}
}

// Trial is part of price identity: two prices with the same recurring substance
// but different first phases (one with a trial, one without) are distinct rows
// under the unique key, so the loader must accept both.
func TestLoad_AcceptsSameTermsWithDifferentTrials(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    prices:
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, psps: [mobius]}
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, psps: [mobius], trial: {unit_amount: 100, duration: 7d}}
`
	if _, err := Load(writeManifest(t, body)); err != nil {
		t.Fatalf("same terms with different trials must be accepted: %v", err)
	}
}

func TestLoad_DuplicateProductKey(t *testing.T) {
	body := `
version: 1
products:
  - {key: p, display_name: P, tier_group: g1, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: p, display_name: P, tier_group: g2, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "duplicate product key") {
		t.Fatalf("want duplicate product key error, got %v", err)
	}
}

func TestLoad_MissingTierRank(t *testing.T) {
	body := `
version: 1
products:
  - {key: p1, display_name: P1, tier_group: g, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: p2, display_name: P2, tier_group: g, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "tier_rank is required") {
		t.Fatalf("want tier_rank error, got %v", err)
	}
}

func TestLoad_TierRankOptionalForSingleProduct(t *testing.T) {
	body := `
version: 1
products:
  - {key: p, display_name: P, tier_group: g, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.TierGroups[0].Products[0].TierRank != nil {
		t.Fatalf("single-product tier_rank should stay omitted")
	}
}

func TestLoad_TierRankAllowsZeroAndNegative(t *testing.T) {
	body := `
version: 1
products:
  - {key: free, display_name: Free, tier_group: g, tier_rank: -1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: starter, display_name: Starter, tier_group: g, tier_rank: 0, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: pro, display_name: Pro, tier_group: g, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ranks := ranksByKey(m)
	if ranks["free"] != -1 || ranks["starter"] != 0 || ranks["pro"] != 1 {
		t.Fatalf("unexpected ranks: %v", ranks)
	}
}

func TestLoad_TierRankDirectionSurvivesRenumberAndNegativePrepend(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "renumber",
			body: `
version: 1
products:
  - {key: starter, display_name: Starter, tier_group: g, tier_rank: 10, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: pro, display_name: Pro, tier_group: g, tier_rank: 20, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
`,
		},
		{
			name: "prepend-negative",
			body: `
version: 1
products:
  - {key: free, display_name: Free, tier_group: g, tier_rank: -1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: starter, display_name: Starter, tier_group: g, tier_rank: 0, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
  - {key: pro, display_name: Pro, tier_group: g, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Load(writeManifest(t, tt.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			ranks := ranksByKey(m)
			if !(ranks["starter"] < ranks["pro"]) {
				t.Fatalf("starter should downgrade from pro and pro should upgrade from starter: %v", ranks)
			}
		})
	}
}

func ranksByKey(m *Manifest) map[string]int {
	ranks := map[string]int{}
	for _, group := range m.TierGroups {
		for _, product := range group.Products {
			ranks[product.Key] = product.tierRank()
		}
	}
	return ranks
}

func TestLoad_SolanaNonStablecoinRejected(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: eur, unit_amount: 1000, duration: 30d, psps: [solana]}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "solana requires a stablecoin") {
		t.Fatalf("want solana eligibility error, got %v", err)
	}
}

func TestLoad_FlatProductsBenefitsUsageLimitsAndRateCards(t *testing.T) {
	body := `
version: 1
usage_limits:
  - key: starter-spend
    measure: billable_spend
    windows:
      - {window: 5h, amount: 10_000_000}
meters:
  - key: api-calls
    aggregation: sum
    value_property: $.count
credit_balances:
  - key: monthly-usd
    unit: usd
  - key: ai-images
    unit: local-stack/ai-image-credit
products:
  - key: premium
    display_name: Premium
    entitlements: [premium]
    usage_limits: [starter-spend]
    credits:
      - key: monthly-usd
        amount: 25_000_000
        expires: 30d
        cadence: per_renewal
      - key: ai-images
        amount: 100
    rate_cards:
      - meter: api-calls
        payment_term: in_arrears
        price:
          model: per_unit
          currency: usd
          per_unit: {unit_amount: 200_000, divide_by: 1_000_000}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.TierGroups) != 1 || m.TierGroups[0].Key != "usage-premium" {
		t.Fatalf("flat products were not normalized: %+v", m.TierGroups)
	}
	p := m.TierGroups[0].Products[0]
	if p.Key != "premium" || p.UsageLimits[0] != "starter-spend" {
		t.Fatalf("product benefits not normalized: %+v", p)
	}
	// or#883: a grant's unit is always the named balance's unit — never declared.
	if got := p.Credits[0].Unit; got != "USD" {
		t.Fatalf("credit did not derive balance unit: %q", got)
	}
	if got := p.Credits[0].ExpiryHours; got == nil || *got != 30*24 {
		t.Fatalf("credit expires did not populate expiry hours: %v", got)
	}
	if got := p.Credits[1].Unit; got != "local-stack/ai-image-credit" {
		t.Fatalf("credit did not derive qualified balance unit: %q", got)
	}
	// A pure-usage product carries no price row — only the rate card.
	if len(p.Prices) != 0 {
		t.Fatalf("pure-usage product should declare no price row, got %+v", p.Prices)
	}
	if len(p.RateCards) != 1 {
		t.Fatalf("expected one rate card, got %+v", p.RateCards)
	}
	rc := p.RateCards[0]
	if rc.Meter != "api-calls" || rc.PaymentTerm != PaymentInArrears {
		t.Fatalf("rate card meter/term wrong: %+v", rc)
	}
	if rc.Price.Model != ModelPerUnit || rc.Price.PerUnit == nil ||
		rc.Price.PerUnit.UnitAmount != 200_000 || rc.Price.PerUnit.DivideBy != 1_000_000 {
		t.Fatalf("rate card price wrong: %+v", rc.Price)
	}
	if rc.Price.Currency != "USD" {
		t.Fatalf("rate card currency wrong: %q", rc.Price.Currency)
	}
}

// The former gauge shape ({kind: gauge} + metered.per) is expressed canonically:
// divide_by carries per_units x per-seconds directly. Same integer math, one input.
func TestLoad_RateCardCarriesTimeDenominatorInDivideBy(t *testing.T) {
	body := `
version: 1
meters:
  - {key: vm-seconds, aggregation: sum, value_property: $.seconds}
products:
  - key: vm
    display_name: VM
    rate_cards:
      - meter: vm-seconds
        payment_term: in_arrears
        price:
          model: per_unit
          currency: usd
          per_unit: {unit_amount: 500_000, divide_by: 3600}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := m.TierGroups[0].Products[0]
	if len(p.RateCards) != 1 {
		t.Fatalf("expected one rate card, got %+v", p.RateCards)
	}
	pu := p.RateCards[0].Price.PerUnit
	if pu == nil || pu.UnitAmount != 500_000 || pu.DivideBy != 3600 {
		t.Fatalf("per-unit denominator wrong: %+v", pu)
	}
}

// A subscription base fee and a usage rate card coexist on one product: the
// price row is the recurring term, the rate card is the arrears usage.
func TestLoad_BaseFeePriceRowCoexistsWithRateCard(t *testing.T) {
	body := `
version: 1
meters:
  - {key: api-calls, aggregation: sum, value_property: $.count}
products:
  - key: api
    display_name: API
    prices:
      - currency: usd
        unit_amount: 5_000_000
        duration: 30d
        auto_renew: true
        psps: []
    rate_cards:
      - meter: api-calls
        payment_term: in_arrears
        price:
          model: per_unit
          currency: usd
          per_unit: {unit_amount: 2_000}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := m.TierGroups[0].Products[0]
	if len(p.Prices) != 1 || p.Prices[0].UnitAmount != 5_000_000 {
		t.Fatalf("base-fee price row not preserved: %+v", p.Prices)
	}
	if len(p.RateCards) != 1 || p.RateCards[0].Meter != "api-calls" {
		t.Fatalf("expected the usage rate card: %+v", p.RateCards)
	}
}

// One meter backs at most one usage rate card, whichever product declares it.
func TestLoad_RateCardsMayNotShareAMeter(t *testing.T) {
	body := `
version: 1
meters:
  - {key: droplet-vcpu-seconds, aggregation: sum, value_property: $.seconds}
products:
  - key: basic-droplet
    display_name: Basic Droplet
    rate_cards:
      - meter: droplet-vcpu-seconds
        payment_term: in_arrears
        price: {model: per_unit, currency: usd, per_unit: {unit_amount: 7_000, divide_by: 3_600}}
  - key: premium-droplet
    display_name: Premium Droplet
    rate_cards:
      - meter: droplet-vcpu-seconds
        payment_term: in_arrears
        price: {model: per_unit, currency: usd, per_unit: {unit_amount: 14_000, divide_by: 3_600}}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "one meter per usage rate card") {
		t.Fatalf("want shared-meter rejection, got %v", err)
	}
}

// or#893 phase 5: the rate card is THE pricing input. The #599 metered: sugar
// and the counter|gauge meter kind are gone; a manifest that still declares
// either fails loudly, and the error carries the rewrite.
func TestLoad_RetiredPricingInputsFailWithTheRewrite(t *testing.T) {
	t.Run("metered: price sugar", func(t *testing.T) {
		body := `
version: 1
meters:
  - {key: api-calls, aggregation: sum, value_property: $.count}
products:
  - key: api
    display_name: API
    prices:
      - currency: usd
        unit_amount: 0
        psps: []
        metered: {meter: api-calls, rate: 2_000}
`
		_, err := Load(writeManifest(t, body))
		if err == nil {
			t.Fatal("a metered: price must not load")
		}
		for _, want := range []string{
			`unknown field "metered"`,
			"the metered: price sugar was removed (or#893/#707)",
			"rate_cards:",
			"payment_term: in_arrears",
			"model: per_unit",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error must carry %q, got:\n%v", want, err)
			}
		}
	})

	t.Run("meter kind:", func(t *testing.T) {
		body := `
version: 1
meters:
  - {key: api-calls, kind: counter}
products:
  - key: api
    display_name: API
    prices:
      - {currency: usd, unit_amount: 1_000, duration: 30d}
`
		_, err := Load(writeManifest(t, body))
		if err == nil {
			t.Fatal("a kind: meter must not load")
		}
		for _, want := range []string{
			`unknown field "kind"`,
			"meter kind: counter|gauge was removed (or#893/#707)",
			"aggregation: sum",
			"aggregation: count",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error must carry %q, got:\n%v", want, err)
			}
		}
	})

	t.Run("meter without aggregation", func(t *testing.T) {
		body := `
version: 1
meters:
  - {key: api-calls}
products:
  - key: api
    display_name: API
    prices:
      - {currency: usd, unit_amount: 1_000, duration: 30d}
`
		_, err := Load(writeManifest(t, body))
		if err == nil || !strings.Contains(err.Error(), "aggregation is required") {
			t.Fatalf("want missing-aggregation error, got %v", err)
		}
	})
}

func TestLoad_UsageLimitReferenceMustExist(t *testing.T) {
	body := `
version: 1
products:
  - key: api
    display_name: API
    usage_limits: [missing]
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "unknown usage_limit") {
		t.Fatalf("want usage_limit reference error, got %v", err)
	}
}

func TestLoad_SolanaStablecoinAccepted(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usdc, unit_amount: 1000, duration: 30d, auto_renew: true, psps: [solana]}
`
	if _, err := Load(writeManifest(t, body)); err != nil {
		t.Fatalf("usdc + solana should be accepted, got %v", err)
	}
}

func TestLoad_BadInterval(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 1w}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "must use h or d") {
		t.Fatalf("want interval error, got %v", err)
	}
}

func TestLoad_RejectsLegacyNamedInterval(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: month}
`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "whole h or d value") {
		t.Fatalf("want duration interval error, got %v", err)
	}
}

func TestLoad_NormalizesWholeDayHourInterval(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 24h, auto_renew: true}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.TierGroups[0].Products[0].Prices[0].Duration; got != "1d" {
		t.Fatalf("duration = %q, want 1d", got)
	}
}

func TestLoad_AcceptsSubDayRecurringInterval(t *testing.T) {
	body := `
version: 1
products:
  - key: p
    display_name: P
    tier_group: g
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1000, duration: 1h, auto_renew: true}
`
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.TierGroups[0].Products[0].Prices[0].Duration; got != "1h" {
		t.Fatalf("duration = %q, want 1h", got)
	}
}
