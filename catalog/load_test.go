package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func parse(t *testing.T, body string) *Manifest {
	t.Helper()
	m, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

func product(t *testing.T, m *Manifest, key string) Product {
	t.Helper()
	for _, g := range m.TierGroups {
		for _, p := range g.Products {
			if p.Key == key {
				return p
			}
		}
	}
	t.Fatalf("product %q not found", key)
	return Product{}
}

func TestLoadNormalizesDeclarations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(path, []byte(`
version: 1
products:
  - key: Initiate
    display_name: Novice
    tier_group: Membership
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1200, duration: 720h, auto_renew: true, psps: [Stripe, stripe, " "]}
  - key: craftsman
    display_name: Craftsman
    tier_group: membership
    tier_rank: 2
    prices:
      - {currency: usd, unit_amount: 2900, duration: 30d, auto_renew: true, psps: [stripe]}
      - currency: usd
        unit_amount: 1500
        duration: 30d
        auto_renew: true
        archived: true
        psps: [stripe]
        psp_links: {stripe: {price_id: price_legacy123}}
  - key: ebook
    display_name: Ebook
    prices:
      - {currency: usdc, unit_amount: 500, psps: [solana]}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.TierGroups) != 2 || m.TierGroups[0].Key != "membership" || len(m.TierGroups[0].Products) != 2 || m.TierGroups[1].Key != "default" {
		t.Fatalf("grouping: %+v", m.TierGroups)
	}
	initiate := product(t, m, "initiate")
	if p := initiate.Prices[0]; p.Currency != "USD" || p.Duration != "30d" || !reflect.DeepEqual(p.PSPs, []string{"stripe"}) {
		t.Fatalf("price not normalized (upper currency, whole-day duration, deduped psps): %+v", p)
	}
	legacy := product(t, m, "craftsman").Prices[1]
	if !legacy.Archived || legacy.PSPLinks["stripe"]["price_id"] != "price_legacy123" {
		t.Fatalf("archived price or psp link lost: %+v", legacy)
	}
	if p := product(t, m, "ebook").Prices[0]; p.Duration != "indefinite" || p.Currency != "USDC" {
		t.Fatalf("omitted duration must mean indefinite: %+v", p)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate must be idempotent on a loaded manifest: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing file loaded")
	}
}

func TestParseAcceptsPriceTerms(t *testing.T) {
	for _, tc := range []struct {
		name, prices, duration string
		count                  int
	}{
		{"same terms, different trial", `
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, psps: [mobius]}
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, psps: [mobius], trial: {unit_amount: 0, duration: 7d}}`, "30d", 2},
		{"solana stablecoin", `
      - {currency: usdg, unit_amount: 1000, duration: 30d, auto_renew: true, psps: [solana]}`, "30d", 1},
		{"24h is one day", `
      - {currency: usd, unit_amount: 1000, duration: 24h, auto_renew: true}`, "1d", 1},
		{"sub-day recurring", `
      - {currency: usd, unit_amount: 1000, duration: 1h, auto_renew: true}`, "1h", 1},
		{"non-day hours kept in hours", `
      - {currency: usd, unit_amount: 1000, duration: 36h, auto_renew: true}`, "36h", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parse(t, "version: 1\nproducts:\n  - key: p\n    display_name: P\n    tier_group: g\n    prices:"+tc.prices)
			prices := m.TierGroups[0].Products[0].Prices
			if len(prices) != tc.count {
				t.Fatalf("price count=%d, want %d", len(prices), tc.count)
			}
			for _, price := range prices {
				if price.Duration != tc.duration {
					t.Errorf("duration=%q, want %q", price.Duration, tc.duration)
				}
			}
		})
	}
}

func TestParseTierRanksKeepDirection(t *testing.T) {
	price := `prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]`
	single := parse(t, "version: 1\nproducts:\n  - {key: p, display_name: P, tier_group: g, "+price+"}\n")
	if single.TierGroups[0].Products[0].TierRank != nil {
		t.Fatal("single-product group rank must stay omitted")
	}
	for name, ranks := range map[string][3]int{"prepended negative": {-1, 0, 1}, "sparse": {10, 20, 30}} {
		body := "version: 1\nproducts:\n"
		for i, key := range []string{"free", "starter", "pro"} {
			body += "  - {key: " + key + ", display_name: X, tier_group: g, tier_rank: " + strconv.Itoa(ranks[i]) + ", " + price + "}\n"
		}
		m := parse(t, body)
		for i, key := range []string{"free", "starter", "pro"} {
			if got := product(t, m, key).tierRank(); got != ranks[i] {
				t.Errorf("%s: rank[%s]=%d, want %d", name, key, got, ranks[i])
			}
		}
	}
}

func TestParseRefusesInvalidManifests(t *testing.T) {
	const tier = "    tier_group: g\n    tier_rank: 1\n"
	prices := func(p ...string) string {
		return "version: 1\nproducts:\n  - key: p\n    display_name: P\n    prices:\n      - " + strings.Join(p, "\n      - ") + "\n"
	}
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"bad version", "version: 2\nproducts: []", []string{"unsupported"}},
		{"no products", "version: 1\nproducts: []", []string{"must define products"}},
		{"tier_groups key", "version: 1\ntier_groups: []", []string{"unknown field"}},
		{"deferred credit_balances", "version: 1\ncredit_balances: []\nproducts: []", []string{"unknown field"}},
		{"deferred usage_limits", "version: 1\nusage_limits: []\nproducts: []", []string{"unknown field"}},
		{"default_providers", "version: 1\ndefault_providers: [stripe]\nproducts: []", []string{"unknown field"}},
		{"product credits", "version: 1\nproducts:\n  - {key: p, display_name: P, credits: []}", []string{"unknown field"}},
		{"product includes", "version: 1\nproducts:\n  - {key: p, display_name: P, includes: []}", []string{"unknown field"}},
		{"product status", "version: 1\nproducts:\n  - {key: p, display_name: P, status: archived}", []string{"unknown field"}},
		{"product psps", "version: 1\nproducts:\n  - {key: p, display_name: P, psps: [stripe]}", []string{"unknown field"}},
		{"missing display name", "version: 1\nproducts:\n  - {key: p, prices: [{currency: usd, unit_amount: 1}]}", []string{"display_name is required"}},
		{"retired providers", prices("{currency: usd, unit_amount: 10_000, providers: [stripe]}"), []string{`unknown field "providers"`, "providers: was renamed to psps:"}},
		{"retired provider_links", prices("{currency: usd, unit_amount: 1000, psps: [stripe], provider_links: {stripe: {lookup_key: k}}}"), []string{`unknown field "provider_links"`, "provider_links: was renamed to psp_links:"}},
		{"retired metered", prices("{currency: usd, unit_amount: 0, metered: {meter: api-calls, rate: 2_000}}"), []string{`unknown field "metered"`, "the metered: price sugar was removed (or#893/#707)", "rate_cards:", "payment_term: in_arrears", "model: per_unit"}},
		{"retired meter kind", "version: 1\nmeters:\n  - {key: api-calls, kind: counter}\nproducts: []", []string{`unknown field "kind"`, "meter kind: counter|gauge was removed (or#893/#707)", "aggregation: sum", "aggregation: count"}},
		{"psp link without psp", prices("{currency: usd, unit_amount: 1000, duration: 30d, psp_links: {stripe: {lookup_key: k}}}"), []string{"requires psps to include"}},
		{"duplicate terms", prices("{currency: usd, unit_amount: 1000, duration: 30d, auto_renew: true}", "{currency: USD, unit_amount: 1000, duration: 720h, auto_renew: true}"), []string{"duplicate price terms"}},
		{"terms differ only by psp", prices("{currency: usd, unit_amount: 23000000, duration: 30d, psps: [mobius, ccbill, solana]}", "{currency: usd, unit_amount: 23000000, duration: 30d, psps: [solana], archived: true}"), []string{"duplicate price terms"}},
		{"zero amount", prices("{currency: usd, unit_amount: 0}"), []string{"must be positive"}},
		{"negative amount", prices("{currency: usd, unit_amount: -5}"), []string{"non-negative"}},
		{"missing currency", prices("{unit_amount: 1}"), []string{"currency is required"}},
		{"custom currency", prices("{currency: custom, unit_amount: 1}"), []string{"ISO money currency"}},
		{"non-alpha currency", prices("{currency: us1, unit_amount: 1}"), []string{"ISO money currency"}},
		{"solana non-stablecoin", prices("{currency: eur, unit_amount: 1000, duration: 30d, psps: [solana]}"), []string{"solana requires a stablecoin"}},
		{"week interval", prices("{currency: usd, unit_amount: 1000, duration: 1w}"), []string{"must use h or d"}},
		{"named interval", prices("{currency: usd, unit_amount: 1000, duration: month}"), []string{"whole h or d value"}},
		{"auto_renew indefinite", prices("{currency: usd, unit_amount: 1000, auto_renew: true}"), []string{"auto_renew requires a finite duration"}},
		{"trial without renew", prices("{currency: usd, unit_amount: 1000, duration: 30d, trial: {unit_amount: 0, duration: 7d}}"), []string{"trial requires auto_renew"}},
		{"indefinite trial", prices("{currency: usd, unit_amount: 1000, duration: 30d, auto_renew: true, trial: {unit_amount: 0, duration: indefinite}}"), []string{"trial.duration must be a finite duration"}},
		{"negative trial", prices("{currency: usd, unit_amount: 1000, duration: 30d, auto_renew: true, trial: {unit_amount: -1, duration: 7d}}"), []string{"trial.unit_amount must be >= 0"}},
		{"tier without recurring price", "version: 1\nproducts:\n  - key: p\n    display_name: P\n" + tier + "    prices: [{currency: usd, unit_amount: 1}]", []string{"membership tier requires a recurring price"}},
		{"duplicate product key", "version: 1\nproducts:\n  - {key: p, display_name: P, prices: [{currency: usd, unit_amount: 1}]}\n  - {key: P, display_name: P, prices: [{currency: usd, unit_amount: 2}]}", []string{"duplicate product key"}},
		{"missing tier rank", "version: 1\nproducts:\n  - {key: a, display_name: A, tier_group: g, tier_rank: 1, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}\n  - {key: b, display_name: B, tier_group: g, prices: [{currency: usd, unit_amount: 1, duration: 30d, auto_renew: true}]}", []string{"tier_rank is required"}},
		{"missing aggregation", "version: 1\nmeters:\n  - {key: api-calls}\nproducts:\n  - {key: p, display_name: P, prices: [{currency: usd, unit_amount: 1}]}", []string{"aggregation is required"}},
		{"bad aggregation", "version: 1\nmeters:\n  - {key: m, event_type: e, value_property: $.v, aggregation: median}\nproducts:\n  - {key: p, display_name: P, prices: [{currency: usd, unit_amount: 1}]}", []string{"aggregation must be one of"}},
		{"duplicate meter", "version: 1\nmeters:\n  - {key: m, aggregation: count}\n  - {key: M, aggregation: count}\nproducts:\n  - {key: p, display_name: P, prices: [{currency: usd, unit_amount: 1}]}", []string{"duplicate meter key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if err == nil {
				t.Fatal("invalid catalog accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must contain %q, got %v", want, err)
				}
			}
		})
	}
}

func TestValidateRequiresProductsNotTierGroups(t *testing.T) {
	var nilManifest *Manifest
	if err := nilManifest.Validate(); err == nil {
		t.Fatal("nil manifest validated")
	}
	m := &Manifest{Version: SupportedVersion, TierGroups: []TierGroup{{Key: "old", Products: []Product{{Key: "p", DisplayName: "P"}}}}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "not tier_groups") {
		t.Fatalf("want tier_groups-only rejection, got %v", err)
	}
}
