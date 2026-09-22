package postgresmigrations

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestOwnedTablesMatchMigrationsAndRestoreScope(t *testing.T) {
	sql := loadAllSchema(t)
	tables := map[string]bool{}
	for _, match := range reCreateTable.FindAllStringSubmatch(sql, -1) {
		tables[match[1]] = true
	}
	for _, match := range reDropTable.FindAllStringSubmatch(sql, -1) {
		delete(tables, match[1])
	}
	for _, match := range reRenameTable.FindAllStringSubmatch(sql, -1) {
		delete(tables, match[1])
		tables[match[2]] = true
	}
	if len(tables) < 50 || len(tables) != len(OwnedTables) {
		t.Fatalf("owned table inventory drift: schema=%d inventory=%d", len(tables), len(OwnedTables))
	}
	definitions := regexp.MustCompile(`(?m)^CREATE(?: OR REPLACE)? FUNCTION openrails.guard_billing_restore_receipt\(\)`).FindAllStringIndex(sql, -1)
	if len(definitions) == 0 {
		t.Fatal("restore guard definition is missing")
	}
	start := definitions[len(definitions)-1][0]
	end := strings.Index(sql[start:], "\n$$;")
	if end < 0 {
		t.Fatal("restore guard body is incomplete")
	}
	migration := []byte(sql[start : start+end])
	for name := range tables {
		if !slices.Contains(OwnedTables, name) {
			t.Errorf("new OpenRails table %s needs an explicit ownership/archive decision", name)
		}
		if !regexp.MustCompile(`'` + regexp.QuoteMeta(name) + `'`).Match(migration) {
			t.Errorf("restore scope lacks owned table %s", name)
		}
	}
}

func TestOwnedViewsMatchMigrations(t *testing.T) {
	sql, err := FS.ReadFile(BaselineName)
	if err != nil {
		t.Fatal(err)
	}
	views := regexp.MustCompile(`(?m)^CREATE VIEW openrails\.([a-z_]+)`).FindAllStringSubmatch(string(sql), -1)
	if len(views) != len(OwnedViews) {
		t.Fatalf("owned view inventory drift: schema=%d inventory=%d", len(views), len(OwnedViews))
	}
	for _, view := range views {
		if !slices.Contains(OwnedViews, view[1]) {
			t.Errorf("view %s lacks ownership decision", view[1])
		}
	}
}
