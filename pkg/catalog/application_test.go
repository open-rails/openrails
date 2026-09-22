package catalog

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestApplicationFormatsPreserveIntent(t *testing.T) {
	yaml := []byte(`schema_version: 1
application_id: deployment-a
expected_revision: 0
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
`)
	a, err := ParseApplicationYAML(yaml)
	if err != nil {
		t.Fatal(err)
	}
	p := a.Products[0]
	if !p.Archived.Set || p.Archived.Value || p.Description.Set || !p.TierGroup.Null || !p.EntitlementsSpec.Set || p.EntitlementsSpec.Null {
		t.Fatalf("lost field presence: %+v", p)
	}
	if got := p.Prices[0].UnitAmount.Value; got != 9007199254740993 {
		t.Fatalf("money rounded: %d", got)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"unit_amount":"9007199254740993"`) || strings.Contains(string(raw), `"description"`) {
		t.Fatalf("invalid SDK encoding: %s", raw)
	}
	b, err := ParseApplicationJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	ha, err := a.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatal("YAML and JSON have different semantic identity")
	}
	b.Products[0].Archived = Field[bool]{}
	hc, err := b.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	if hc == ha {
		t.Fatal("omitted false collapsed into explicit false")
	}
	// More than forty sibling nodes is a shallow, valid production-sized file.
	for i := 0; i < 30; i++ {
		a.Products = append(a.Products, ApplyProduct{Key: fmt.Sprintf("product-%d", i), DisplayName: Value("Item")})
	}
	raw, err = json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseApplicationYAML(raw); err != nil {
		t.Fatalf("sibling nodes counted as nesting: %v", err)
	}
}

func TestApplicationRejectsAmbiguousInput(t *testing.T) {
	base := "schema_version: 1\napplication_id: operation\nexpected_revision: 0\n"
	for name, suffix := range map[string]string{
		"duplicate field":      "prune: true\nprune: false\n",
		"unknown field":        "merchant_admin: true\n",
		"multiple documents":   "---\nproducts: []\n",
		"alias":                "products: &items []\nmeters: *items\n",
		"tag":                  "products: !!seq []\n",
		"duplicate product":    "products: [{key: a}, {key: a}]\n",
		"fractional money":     "products: [{key: a, prices: [{key: p, unit_amount: 1.1}]}]\n",
		"overflow money":       "products: [{key: a, prices: [{key: p, unit_amount: '9223372036854775808'}]}]\n",
		"unknown nested field": "products: [{key: a, display_nmae: nope}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseApplicationYAML([]byte(base + suffix)); err == nil {
				t.Fatal("invalid YAML accepted")
			}
		})
	}
	for _, raw := range []string{
		`{"schema_version":1,"application_id":"op","expected_revision":0,"prune":true,"prune":false}`,
		`{"schema_version":1,"application_id":"op","expected_revision":0,"prune":false,"Prune":true}`,
		`{"schema_version":1,"application_id":"op","expected_revision":0,"products":[{"key":"p","Archived":true}]}`,
		`{"schema_version":1,"application_id":"op","expected_revision":0,"products":[{"key":"a","entitlements_spec":{"premium":1,"premium":2}}]}`,
		`{"schema_version":1,"application_id":"op","expected_revision":0} {}`,
	} {
		if _, err := ParseApplicationJSON([]byte(raw)); err == nil {
			t.Fatalf("ambiguous JSON accepted: %s", raw)
		}
	}
	if _, err := ParseApplicationJSON([]byte(strings.Repeat("[", 34) + strings.Repeat("]", 34))); err == nil {
		t.Fatal("excessive JSON depth accepted")
	}
}

func TestApplicationCanonicalOrder(t *testing.T) {
	revision := int64(0)
	a := Application{SchemaVersion: 1, ApplicationID: "op", ExpectedRevision: &revision, Products: []ApplyProduct{{Key: "b"}, {Key: "a", Prices: []ApplyPrice{{Key: "z"}, {Key: "x"}}}}}
	ha, err := a.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	if a.Products[0].Key != "b" || a.Products[1].Prices[0].Key != "z" {
		t.Fatal("hash mutated request")
	}
	b := a
	b.Products = []ApplyProduct{a.Products[1], a.Products[0]}
	hb, err := b.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatal("order of independent declarations changed identity")
	}
	b.Prune = true
	hb, err = b.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	if ha == hb {
		t.Fatal("prune was not included in identity")
	}
}

func TestApplicationRejectsHiddenGoValues(t *testing.T) {
	revision := int64(0)
	for _, hidden := range []Field[map[string]*int]{
		{Value: map[string]*int{"premium": nil}},
		{Set: true, Null: true, Value: map[string]*int{"premium": nil}},
		{Null: true},
	} {
		a := Application{SchemaVersion: 1, ApplicationID: "op", ExpectedRevision: &revision, Products: []ApplyProduct{{Key: "p", EntitlementsSpec: hidden}}}
		if err := a.Validate(); err == nil {
			t.Fatal("typed operator request accepted a value absent from its wire representation")
		}
		if _, err := a.CanonicalDigest(); err == nil {
			t.Fatal("hidden value acquired a receipt digest")
		}
	}
}
