package currency

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The schema's checks that tell a registry currency from a merchant's unit
// list the registry: they change with it.
func TestSchemaListsTheRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../api/schema.txt")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, u := range List() {
		want = append(want, u.Code)
	}
	arrays := regexp.MustCompile(`ARRAY\[('[A-Z0-9]+'::text(?:, '[A-Z0-9]+'::text)*)\]`)
	codes := regexp.MustCompile(`'([A-Z0-9]+)'`)
	found := 0
	for _, line := range strings.Split(string(raw), "\n") {
		for _, m := range arrays.FindAllStringSubmatch(line, -1) {
			var got []string
			for _, c := range codes.FindAllStringSubmatch(m[1], -1) {
				got = append(got, c[1])
			}
			if !slices.Contains(got, "USD") || !slices.Contains(got, "AED") {
				continue
			}
			found++
			if !slices.Equal(got, want) {
				t.Errorf("%s lists %v, the registry %v", strings.TrimSpace(line)[:min(80, len(strings.TrimSpace(line)))], got, want)
			}
		}
	}
	if found < 3 {
		t.Fatalf("found %d registry lists in the schema, want the unit checks' at least", found)
	}
}
