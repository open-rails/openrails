package merchantarchive

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// A table a migration adds without an inventory decision would be invisible
// to checkSchema, which judges only ownedTables.
func TestOwnedTablesMatchSchemaSnapshot(t *testing.T) {
	raw, err := os.ReadFile("../../api/schema.txt")
	if err != nil {
		t.Fatal(err)
	}
	var schema []string
	for _, m := range regexp.MustCompile(`(?m)^table billing\.(\w+)`).FindAllSubmatch(raw, -1) {
		schema = append(schema, string(m[1]))
	}
	for _, name := range schema {
		if !slices.Contains(ownedTables, name) {
			t.Errorf("billing.%s is in api/schema.txt but not in ownedTables: decide whether the archive carries it", name)
		}
	}
	for _, name := range ownedTables {
		if !slices.Contains(schema, name) {
			t.Errorf("ownedTables names %s, which api/schema.txt does not list", name)
		}
	}
}
