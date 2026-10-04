package merchantarchive

import (
	"errors"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

func TestEveryOwnedTableHasOneArchiveDecision(t *testing.T) {
	owned := map[string]bool{}
	for _, name := range ownedTables {
		owned[name] = true
	}
	decided := map[string]bool{}
	for _, p := range contract.Profiles {
		decided[p.Name] = true
		// The wire binds every row to the archive's merchant via column 0.
		if len(p.Columns) == 0 || p.Columns[0] != (contract.Column{Name: "merchant_id", Type: "uuid"}) {
			t.Errorf("profile %s must lead with merchant_id uuid", p.Name)
		}
		if _, excluded := excludedTables[p.Name]; excluded {
			t.Errorf("table %s is both archived and excluded", p.Name)
		}
		if !strings.Contains(exportQuery(p), "WHERE t.merchant_id=$1") {
			t.Errorf("export of %s is not merchant scoped", p.Name)
		}
	}
	for name := range excludedTables {
		decided[name] = true
	}
	for name := range owned {
		if !decided[name] {
			t.Errorf("owned table %s requires an explicit archive decision", name)
		}
	}
	for name := range decided {
		if !owned[name] {
			t.Errorf("archive claims non-owned table %s", name)
		}
	}
}

func TestClassifyMapsDatabaseRefusals(t *testing.T) {
	for code, want := range map[string]string{"55000": "not_empty", "23505": "integrity", "23503": "integrity", "P0002": "merchant_mismatch", "40001": "database"} {
		var archiveErr *Error
		err := classify(&pgconn.PgError{Code: code})
		if !errors.As(err, &archiveErr) || archiveErr.Code != want {
			t.Errorf("%s: got %v, want %s", code, err, want)
		}
	}
	original := &Error{Code: "not_empty", Table: "payments", Count: 3}
	if classify(original) != original || classify(nil) != nil {
		t.Fatal("classify must pass archive errors and nil through")
	}
}

// Directory identity rows are not billing data: a restore destination may hold
// them (a renamed merchant, a host claim) and still be empty.
var restoreOccupancyExempt = []string{"merchant_api_host_claims", "merchant_slug_aliases"}

var (
	createTable    = regexp.MustCompile(`(?s)CREATE TABLE billing\.(\w+) \((.*?)\n\);`)
	merchantColumn = regexp.MustCompile(`(?m)^\s*merchant_id uuid\b`)
	restoreList    = regexp.MustCompile(`(?s)FUNCTION billing\.guard_billing_restore_receipt\(\).*?AND c\.relname = ANY\(ARRAY\[(.*?)\]::text\[\]\)`)
	quoted         = regexp.MustCompile(`'(\w+)'`)
)

// Every table the migrations create is owned (and so has an archive decision),
// and the restore guard's occupancy list names every merchant-scoped table: a
// table added without either fails here, not at a restore into a busy merchant.
func TestOwnedTablesAndRestoreOccupancyCoverTheSchema(t *testing.T) {
	var all, scoped []string
	var guard []string
	err := fs.WalkDir(postgresmigrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(postgresmigrations.FS, path)
		if err != nil {
			return err
		}
		for _, m := range createTable.FindAllStringSubmatch(string(b), -1) {
			all = append(all, m[1])
			if merchantColumn.MatchString(m[2]) {
				scoped = append(scoped, m[1])
			}
		}
		if m := restoreList.FindStringSubmatch(string(b)); m != nil {
			guard = nil
			for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
				guard = append(guard, q[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || len(guard) == 0 {
		t.Fatalf("parsed %d tables and %d restore-guard entries from the migrations", len(all), len(guard))
	}
	owned := slices.Clone(ownedTables)
	sort.Strings(all)
	sort.Strings(owned)
	if !slices.Equal(all, owned) {
		t.Errorf("ownedTables must list exactly the migrations' tables\nmigrations: %v\nowned:      %v", all, owned)
	}
	var want []string
	for _, name := range scoped {
		if !slices.Contains(restoreOccupancyExempt, name) {
			want = append(want, name)
		}
	}
	sort.Strings(want)
	sort.Strings(guard)
	if !slices.Equal(want, guard) {
		t.Errorf("guard_billing_restore_receipt must check every merchant-scoped table\nwant: %v\ngot:  %v", want, guard)
	}
}
