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
  membership:
    display_name: Membership
    archived: false
    tier_group: null
    entitlements: []
    prices:
      monthly:
        unit_amount: 9007199254740993
        billing_interval: null
        trial_unit_amount: null
`))
	if err != nil {
		t.Fatal(err)
	}
	p := a.Products["membership"]
	if !p.Archived.Set || p.Archived.Value || p.Description.Set || !p.TierGroup.Null || !p.Entitlements.Set || p.Entitlements.Null {
		t.Fatalf("field presence lost: %+v", p)
	}
	if got := p.Prices["monthly"].UnitAmount.Value; got != 9007199254740993 {
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
	p = b.Products["membership"]
	p.Archived = Field[bool]{}
	b.Products["membership"] = p
	if digest(t, *a) == digest(t, *b) {
		t.Fatal("omitted archived collapsed into explicit false")
	}

	// Wide, shallow files must not trip the nesting limit.
	for i := 0; i < 60; i++ {
		a.Products[fmt.Sprintf("product-%d", i)] = ApplyProduct{DisplayName: Value("Item")}
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
		"multiple documents":      "---\nproducts: {}\n",
		"alias":                   "products: &items {}\nmeters: *items\n",
		"tag":                     "products: !!map {}\n",
		"fractional money":        "products: {a: {prices: {p: {unit_amount: 1.1}}}}\n",
		"overflow money":          "products: {a: {prices: {p: {unit_amount: '9223372036854775808'}}}}\n",
		"unknown nested field":    "products: {a: {display_nmae: nope}}\n",
		"padded product key":      "products: {' a': {display_name: A}}\n",
		"long price key":          "products: {a: {prices: {" + strings.Repeat("p", 256) + ": {amount: 1 USD}}}}\n",
	} {
		if _, err := ParseApplicationYAML([]byte(base + suffix)); err == nil {
			t.Errorf("%s: YAML accepted", name)
		}
	}
	head := `{"schema_version":1`
	for name, raw := range map[string]string{
		"duplicate field":         head + `,"prune":true,"prune":false}`,
		"case alias":              head + `,"prune":false,"Prune":true}`,
		"nested case alias":       head + `,"products":{"p":{"Archived":true}}}`,
		"removed entitlement map": head + `,"products":{"a":{"entitlements":{"premium":1,"premium":2}}}}`,
		"trailing document":       head + `} {}`,
		"excessive depth":         strings.Repeat("[", 34) + strings.Repeat("]", 34),
		"empty":                   ``,
		"null money element":      head + `,"products":{"a":{"prices":{"p":{"unit_amount":null}}}}}`,
		"negative money":          head + `,"products":{"a":{"prices":{"p":{"unit_amount":"-1"}}}}}`,
		"schema version 2":        `{"schema_version":2}`,
		"removed application id":  `{"schema_version":1,"application_id":"op"}`,
		"removed revision":        `{"schema_version":1,"expected_revision":0}`,
		"removed catalog version": `{"schema_version":1,"catalog_version":1}`,
		"missing schema":          `{"products":{}}`,
		"oversized document":      head + `,"catalog_id":"` + strings.Repeat("x", MaxApplicationBytes) + `"}`,
		"empty product key":       head + `,"products":{"":{"display_name":"A"}}}`,
		"key field":               head + `,"products":{"a":{"key":"a"}}}`,
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
	manyMeters.Meters = map[string]ApplyMeter{}
	for i := 0; i <= MaxApplicationItems; i++ {
		manyMeters.Meters[fmt.Sprintf("m%d", i)] = ApplyMeter{}
	}
	for name, mutate := range map[string]func(*Application){
		"empty product key": func(a *Application) { a.Products = map[string]ApplyProduct{"": {}} },
		"padded meter key":  func(a *Application) { a.Meters = map[string]ApplyMeter{"m ": {}} },
		"long price key": func(a *Application) {
			a.Products = map[string]ApplyProduct{"p": {Prices: map[string]ApplyPrice{strings.Repeat("x", 256): {}}}}
		},
		"null display name": func(a *Application) { a.Products = map[string]ApplyProduct{"p": {DisplayName: Null[string]()}} },
		"null currency": func(a *Application) {
			a.Products = map[string]ApplyProduct{"p": {Prices: map[string]ApplyPrice{"x": {Currency: Null[string]()}}}}
		},
		"negative trial": func(a *Application) {
			a.Products = map[string]ApplyProduct{"p": {Prices: map[string]ApplyPrice{"x": {TrialUnitAmount: Value[int64](-1)}}}}
		},
		"zero access hours": func(a *Application) {
			a.Products = map[string]ApplyProduct{"p": {Prices: map[string]ApplyPrice{"x": {AccessDurationHours: Value(0)}}}}
		},
		"long display name": func(a *Application) {
			a.Products = map[string]ApplyProduct{"p": {DisplayName: Value(strings.Repeat("n", 1025))}}
		},
		"too many items": func(a *Application) { a.Meters = manyMeters.Meters },
		// Direct Go requests must not carry values their JSON form would drop.
		"hidden value on omitted field": func(a *Application) {
			a.Products = map[string]ApplyProduct{"p": {Entitlements: Field[[]string]{Value: []string{"premium"}}}}
		},
		"hidden value on null field": func(a *Application) {
			a.Products = map[string]ApplyProduct{"p": {Entitlements: Field[[]string]{Set: true, Null: true, Value: []string{"premium"}}}}
		},
		"null without set": func(a *Application) {
			a.Products = map[string]ApplyProduct{"p": {Entitlements: Field[[]string]{Null: true}}}
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
	nullable.Products = map[string]ApplyProduct{"p": {TierGroup: Null[string](), Prices: map[string]ApplyPrice{"x": {TrialUnitAmount: Null[int64](), AccessDurationHours: Null[int]()}}}}
	if err := nullable.Validate(); err != nil {
		t.Fatalf("explicit null on nullable fields rejected: %v", err)
	}
}

// Without identity fields the document itself is the application: its JSON
// omits them, and its digest is its content alone.
func TestApplicationIdentityComesFromContent(t *testing.T) {
	a, err := ParseApplicationYAML([]byte("schema_version: 1\nproducts: {a: {display_name: A}}\n"))
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
	a, err := ParseApplicationYAML([]byte(`schema_version: 1
products:
  b: {}
  a:
    prices:
      z: {psps: [stripe, mobius]}
      x: {}
meters: {m2: {}, m1: {}}
`))
	if err != nil {
		t.Fatal(err)
	}
	ha := digest(t, *a)
	if a.Products["a"].Prices["z"].PSPs.Value[0] != "stripe" {
		t.Fatal("digest mutated the request")
	}
	b, err := ParseApplicationJSON([]byte(`{"meters":{"m1":{},"m2":{}},"products":{"a":{"prices":{"x":{},"z":{"psps":["mobius","stripe"]}}},"b":{}},"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if digest(t, *b) != ha {
		t.Fatal("order of independent declarations changed identity")
	}
	b.Prune = true
	if digest(t, *b) == ha {
		t.Fatal("prune is part of identity")
	}
	b.Prune = false
	b.Products["b"] = ApplyProduct{DisplayName: Value("Changed")}
	if digest(t, *b) == ha {
		t.Fatal("changed product content must change identity")
	}
}

// A map key is the record's identity, so a repeated key is refused in both
// syntaxes rather than resolved last-wins.
func TestApplicationRejectsDuplicateKeys(t *testing.T) {
	for name, raw := range map[string]string{
		"product": "schema_version: 1\nproducts:\n  dup: {display_name: A}\n  dup: {display_name: B}\n",
		"price":   "schema_version: 1\nproducts:\n  a:\n    prices:\n      dup: {amount: 1 USD}\n      dup: {amount: 2 USD}\n",
		"meter":   "schema_version: 1\nmeters:\n  dup: {unit: token}\n  dup: {unit: image}\n",
	} {
		_, err := ParseApplicationYAML([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), `mapping key "dup" already defined`) {
			t.Errorf("YAML duplicate %s key: %v", name, err)
		}
	}
	for name, raw := range map[string]string{
		"product": `{"schema_version":1,"products":{"dup":{"display_name":"A"},"dup":{"display_name":"B"}}}`,
		"price":   `{"schema_version":1,"products":{"a":{"prices":{"dup":{"amount":"1 USD"},"dup":{"amount":"2 USD"}}}}}`,
		"meter":   `{"schema_version":1,"meters":{"dup":{},"dup":{}}}`,
	} {
		_, err := ParseApplicationJSON([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), `duplicate or invalid configuration JSON field "dup"`) {
			t.Errorf("JSON duplicate %s key: %v", name, err)
		}
	}
}

// The list form with key: fields was replaced by maps keyed by key; it fails
// with an error naming the new shape.
func TestApplicationRejectsListForm(t *testing.T) {
	for name, raw := range map[string]string{
		"products": "schema_version: 1\nproducts:\n  - key: a\n    display_name: A\n",
		"prices":   "schema_version: 1\nproducts:\n  a:\n    prices:\n      - key: monthly\n        amount: 1 USD\n",
		"meters":   "schema_version: 1\nmeters:\n  - key: tokens\n",
	} {
		_, err := ParseApplicationYAML([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "must be a map keyed by") || !strings.Contains(err.Error(), "not a list") {
			t.Errorf("%s list form: %v", name, err)
		}
	}
	_, err := ParseApplicationJSON([]byte(`{"schema_version":1,"products":[{"key":"a"}]}`))
	if err == nil || !strings.Contains(err.Error(), "products: must be a map keyed by product key") {
		t.Errorf("JSON list form: %v", err)
	}
	_, err = ParseApplicationYAML([]byte("schema_version: 1\nproducts:\n  a:\n    key: a\n"))
	if err == nil || !strings.Contains(err.Error(), "map key is its key") {
		t.Errorf("key field: %v", err)
	}
}

// Maps marshal with sorted keys: one document has one byte form.
func TestApplicationMarshalIsDeterministic(t *testing.T) {
	a := Application{SchemaVersion: 1, Products: map[string]ApplyProduct{}}
	for _, key := range []string{"zeta", "alpha", "mid", "beta"} {
		a.Products[key] = ApplyProduct{DisplayName: Value(key), Prices: map[string]ApplyPrice{"z": {}, "a": {}}}
	}
	want, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(want), `{"schema_version":1,"products":{"alpha":{"display_name":"alpha","prices":{"a":{},"z":{}}},"beta"`) {
		t.Fatalf("keys not sorted: %s", want)
	}
	for range 20 {
		if got, _ := json.Marshal(a); string(got) != string(want) {
			t.Fatalf("marshal is not deterministic:\n%s\n%s", got, want)
		}
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
