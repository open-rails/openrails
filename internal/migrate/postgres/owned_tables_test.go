package postgresmigrations

import (
	"regexp"
	"slices"
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
	migration, err := FS.ReadFile("0004_shared_schema_restore_scope.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for name := range tables {
		if !slices.Contains(OwnedTables, name) {
			t.Errorf("new OpenRails table %s needs an explicit ownership/archive decision", name)
		}
		if !regexp.MustCompile(`'` + regexp.QuoteMeta(name) + `'`).Match(migration) {
			t.Errorf("restore scope lacks owned table %s", name)
		}
	}
}
