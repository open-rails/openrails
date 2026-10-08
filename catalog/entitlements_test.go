package catalog

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestEntitlementsAreOpaqueLists(t *testing.T) {
	input := []string{"post:101", "premium", " private key "}
	got, err := NormalizeEntitlements(input)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{" private key ", "post:101", "premium"}; !reflect.DeepEqual(want, got) {
		t.Fatalf("opaque keys changed: %#v", got)
	}
	if input[0] != "post:101" {
		t.Fatal("normalizing mutated caller input")
	}
	for _, invalid := range [][]string{{""}, {" \t"}, {"premium", "premium"}, {"key\x00"}, {string([]byte{0xff})}} {
		if _, err := NormalizeEntitlements(invalid); err == nil {
			t.Errorf("accepted invalid entitlement list %#v", invalid)
		}
	}
	for _, value := range [][]string{nil, {}} {
		normalized, err := NormalizeEntitlements(value)
		if err != nil || (value == nil) != (normalized == nil) {
			t.Fatalf("nil versus explicit empty lost: input=%#v output=%#v error=%v", value, normalized, err)
		}
	}
	_, err = NormalizeEntitlements([]string{strings.Repeat("x", MaxEntitlementKeyBytes)})
	if err != nil {
		t.Fatalf("maximum-size opaque key refused: %v", err)
	}
	if _, err := NormalizeEntitlements([]string{strings.Repeat("x", MaxEntitlementKeyBytes+1)}); err == nil {
		t.Fatal("overlong opaque key accepted")
	}
}

func TestApplicationEntitlementListContract(t *testing.T) {
	yaml, err := ParseApplicationYAML([]byte("schema_version: 1\nproducts:\n- key: p\n  entitlements: [post:101, premium]\n"))
	if err != nil {
		t.Fatal(err)
	}
	jsonForm, err := ParseApplicationJSON([]byte(`{"schema_version":1,"products":[{"key":"p","entitlements":["premium","post:101"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if digest(t, *yaml) != digest(t, *jsonForm) {
		t.Fatal("entitlement order changed application identity")
	}
	if yaml.Products[0].Entitlements.Value[0] != "post:101" {
		t.Fatal("hashing mutated entitlement order")
	}
	empty, err := ParseApplicationJSON([]byte(`{"schema_version":1,"products":[{"key":"p","entitlements":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	omitted, err := ParseApplicationJSON([]byte(`{"schema_version":1,"products":[{"key":"p"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Products[0].Entitlements.Set || empty.Products[0].Entitlements.Value == nil || omitted.Products[0].Entitlements.Set || digest(t, *empty) == digest(t, *omitted) {
		t.Fatal("clearing entitlements collapsed into preserving them")
	}
	for _, fields := range []string{
		`"entitlements_spec":{"premium":null}`,
		`"entitlements":{"premium":null}`,
		`"entitlements":null`,
		`"entitlements":["premium","premium"]`,
		`"entitlements":[""]`,
		`"entitlements":[null]`,
		`"entitlements":[72]`,
	} {
		raw := fmt.Sprintf(`{"schema_version":1,"products":[{"key":"p",%s}]}`, fields)
		if _, err := ParseApplicationJSON([]byte(raw)); err == nil {
			t.Errorf("accepted invalid entitlement declaration %s", fields)
		}
	}
	encoded, err := json.Marshal(yaml)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	product := wire["products"].([]any)[0].(map[string]any)
	if _, ok := product["entitlements"].([]any); !ok {
		t.Fatalf("entitlements are not a wire list: %s", encoded)
	}
}
