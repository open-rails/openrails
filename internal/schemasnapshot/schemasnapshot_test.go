package schemasnapshot

import (
	"os"
	"path/filepath"
	"testing"
)

// api/schema.txt was taken from the migration files as they are now. This
// needs no database, so it fails beside the Go and HTTP lists; TestSchemaSnapshot
// in ci compares the schema itself.
func TestSchemaSnapshotNamesTheMigrations(t *testing.T) {
	list, err := os.ReadFile(filepath.Join("..", "..", File))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckMigrations(list); err != nil {
		t.Fatal(err)
	}
}

func TestDiffNamesTheObject(t *testing.T) {
	old := []byte("# note\ntable billing.a\n  column id uuid\n  column gone text\n\ntable billing.b\n  column id uuid\n")
	now := []byte("table billing.a\n  column id uuid\n\ntable billing.b\n  column id uuid\n  column added text\n")
	gone, added := Diff(old, now)
	if len(gone) != 1 || gone[0] != "table billing.a: column gone text" {
		t.Errorf("gone = %q", gone)
	}
	if len(added) != 1 || added[0] != "table billing.b: column added text" {
		t.Errorf("added = %q", added)
	}
}
