package catalog

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestApplicationYAMLAndJSONShareOneIdentity(t *testing.T) {
	a, err := ParseApplicationYAML([]byte(`schema_version: 1
products:
  - key: membership
    display_name: Membership
    archived: false
    tier_group: null
    entitlements_spec: {}
    prices:
      - key: monthly
        unit_amount: 9007199254740993
        auto_renew: false
        trial_unit_amount: null
`))
	if err != nil {
		t.Fatal(err)
	}
	p := a.Products[0]
	if !p.Archived.Set || p.Archived.Value || p.Description.Set || !p.TierGroup.Null || !p.EntitlementsSpec.Set || p.EntitlementsSpec.Null {
		t.Fatalf("field presence lost: %+v", p)
	}
	if got := p.Prices[0].UnitAmount.Value; got != 9007199254740993 {
		t.Fatalf("money past 2^53 rounded: %d", got)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"unit_amount":"9007199254740993"`) || strings.Contains(string(raw), `"description"`) {
		t.Fatalf("SDK encoding must quote money and omit absent fields: %s", raw)
	}
	b, err := ParseApplicationJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if digest(t, *a) != digest(t, *b) {
		t.Fatal("YAML and JSON of the same declaration differ in identity")
	}
	b.Products[0].Archived = Field[bool]{}
	if digest(t, *a) == digest(t, *b) {
		t.Fatal("omitted archived collapsed into explicit false")
	}

	// Wide, shallow files must not trip the nesting limit.
	for i := 0; i < 60; i++ {
		a.Products = append(a.Products, ApplyProduct{Key: fmt.Sprintf("product-%d", i), DisplayName: Value("Item")})
	}
	if raw, err = json.Marshal(a); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseApplicationYAML(raw); err != nil {
		t.Fatalf("siblings counted as nesting: %v", err)
	}
}

func TestApplicationRejectsAmbiguousInput(t *testing.T) {
	base := "schema_version: 1\n"
	for name, suffix := range map[string]string{
		"duplicate field":         "prune: true\nprune: false\n",
		"unknown field":           "merchant_admin: true\n",
		"removed application id":  "application_id: operation\n",
		"removed revision":        "expected_revision: 0\n",
		"removed catalog version": "catalog_version: 1\n",
		"multiple documents":      "---\nproducts: []\n",
		"alias":                   "products: &items []\nmeters: *items\n",
		"tag":                     "products: !!seq []\n",
		"duplicate product":       "products: [{key: a}, {key: a}]\n",
		"fractional money":        "products: [{key: a, prices: [{key: p, unit_amount: 1.1}]}]\n",
		"overflow money":          "products: [{key: a, prices: [{key: p, unit_amount: '9223372036854775808'}]}]\n",
		"unknown nested field":    "products: [{key: a, display_nmae: nope}]\n",
	} {
		if _, err := ParseApplicationYAML([]byte(base + suffix)); err == nil {
			t.Errorf("%s: YAML accepted", name)
		}
	}
	head := `{"schema_version":1`
	for name, raw := range map[string]string{
		"duplicate field":         head + `,"prune":true,"prune":false}`,
		"case alias":              head + `,"prune":false,"Prune":true}`,
		"nested case alias":       head + `,"products":[{"key":"p","Archived":true}]}`,
		"duplicate map key":       head + `,"products":[{"key":"a","entitlements_spec":{"premium":1,"premium":2}}]}`,
		"trailing document":       head + `} {}`,
		"excessive depth":         strings.Repeat("[", 34) + strings.Repeat("]", 34),
		"empty":                   ``,
		"null money element":      head + `,"products":[{"key":"a","prices":[{"key":"p","unit_amount":null}]}]}`,
		"negative money":          head + `,"products":[{"key":"a","prices":[{"key":"p","unit_amount":"-1"}]}]}`,
		"schema version 2":        `{"schema_version":2}`,
		"removed application id":  `{"schema_version":1,"application_id":"op"}`,
		"removed revision":        `{"schema_version":1,"expected_revision":0}`,
		"removed catalog version": `{"schema_version":1,"catalog_version":1}`,
		"missing schema":          `{"products":[]}`,
		"oversized document":      head + `,"catalog_id":"` + strings.Repeat("x", MaxApplicationBytes) + `"}`,
		"duplicate price keys":    head + `,"products":[{"key":"a","prices":[{"key":"p"},{"key":"p"}]}]}`,
	} {
		if _, err := ParseApplicationJSON([]byte(raw)); err == nil {
			t.Errorf("%s: JSON accepted", name)
		}
	}
}

func TestApplicationValidateBounds(t *testing.T) {
	ok := func() Application {
		return Application{SchemaVersion: 1}
	}
	if err := ok().Validate(); err != nil {
		t.Fatalf("minimal application: %v", err)
	}
	manyMeters := ok()
	for i := 0; i <= MaxApplicationItems; i++ {
		manyMeters.Meters = append(manyMeters.Meters, ApplyMeter{Key: fmt.Sprintf("m%d", i)})
	}
	for name, mutate := range map[string]func(*Application){
		"empty product key": func(a *Application) { a.Products = []ApplyProduct{{}} },
		"padded meter key":  func(a *Application) { a.Meters = []ApplyMeter{{Key: "m "}} },
		"null display name": func(a *Application) { a.Products = []ApplyProduct{{Key: "p", DisplayName: Null[string]()}} },
		"null currency": func(a *Application) {
			a.Products = []ApplyProduct{{Key: "p", Prices: []ApplyPrice{{Key: "x", Currency: Null[string]()}}}}
		},
		"negative trial": func(a *Application) {
			a.Products = []ApplyProduct{{Key: "p", Prices: []ApplyPrice{{Key: "x", TrialUnitAmount: Value[int64](-1)}}}}
		},
		"zero access hours": func(a *Application) {
			a.Products = []ApplyProduct{{Key: "p", Prices: []ApplyPrice{{Key: "x", AccessDurationHours: Value(0)}}}}
		},
		"long display name": func(a *Application) {
			a.Products = []ApplyProduct{{Key: "p", DisplayName: Value(strings.Repeat("n", 1025))}}
		},
		"too many items": func(a *Application) { a.Meters = manyMeters.Meters },
		// Direct Go requests must not carry values their JSON form would drop.
		"hidden value on omitted field": func(a *Application) {
			a.Products = []ApplyProduct{{Key: "p", EntitlementsSpec: Field[map[string]*int]{Value: map[string]*int{"premium": nil}}}}
		},
		"hidden value on null field": func(a *Application) {
			a.Products = []ApplyProduct{{Key: "p", EntitlementsSpec: Field[map[string]*int]{Set: true, Null: true, Value: map[string]*int{"premium": nil}}}}
		},
		"null without set": func(a *Application) {
			a.Products = []ApplyProduct{{Key: "p", EntitlementsSpec: Field[map[string]*int]{Null: true}}}
		},
	} {
		a := ok()
		mutate(&a)
		if err := a.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := a.CanonicalDigest(); err == nil {
			t.Errorf("%s: acquired a digest", name)
		}
	}
	nullable := ok()
	nullable.Products = []ApplyProduct{{Key: "p", TierGroup: Null[string](), Prices: []ApplyPrice{{Key: "x", TrialUnitAmount: Null[int64](), AccessDurationHours: Null[int]()}}}}
	if err := nullable.Validate(); err != nil {
		t.Fatalf("explicit null on nullable fields rejected: %v", err)
	}
}

// Without identity fields the document itself is the application: its JSON
// omits them, and its digest is its content alone.
func TestApplicationIdentityComesFromContent(t *testing.T) {
	a, err := ParseApplicationYAML([]byte("schema_version: 1\nproducts: [{key: a, display_name: A}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "application_id") || strings.Contains(string(raw), "expected_revision") || strings.Contains(string(raw), "catalog_version") {
		t.Fatalf("declarative wire form carries identity fields: %s", raw)
	}
	same, err := ParseApplicationJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if digest(t, *a) != digest(t, *same) {
		t.Fatal("YAML and JSON forms of one document disagree")
	}
}

func TestApplicationDigestIgnoresDeclarationOrder(t *testing.T) {
	a := Application{SchemaVersion: 1,
		Products: []ApplyProduct{{Key: "b"}, {Key: "a", Prices: []ApplyPrice{{Key: "z", PSPs: Value([]string{"stripe", "mobius"})}, {Key: "x"}}}},
		Meters:   []ApplyMeter{{Key: "m2"}, {Key: "m1"}},
	}
	ha := digest(t, a)
	if a.Products[0].Key != "b" || a.Products[1].Prices[0].Key != "z" || a.Products[1].Prices[0].PSPs.Value[0] != "stripe" || a.Meters[0].Key != "m2" {
		t.Fatal("digest mutated the request")
	}
	b := a
	b.Products = []ApplyProduct{a.Products[1], a.Products[0]}
	b.Products[0].Prices = []ApplyPrice{{Key: "x"}, {Key: "z", PSPs: Value([]string{"mobius", "stripe"})}}
	b.Meters = []ApplyMeter{a.Meters[1], a.Meters[0]}
	if digest(t, b) != ha {
		t.Fatal("order of independent declarations changed identity")
	}
	b.Prune = true
	if digest(t, b) == ha {
		t.Fatal("prune is part of identity")
	}
	b.Prune = false
	b.Products[0].DisplayName = Value("Changed")
	if digest(t, b) == ha {
		t.Fatal("changed product content must change identity")
	}
}

func digest(t *testing.T, a Application) [32]byte {
	t.Helper()
	d, err := a.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}
