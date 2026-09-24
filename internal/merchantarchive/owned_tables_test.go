package merchantarchive

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

func TestEveryOwnedTableHasOneArchiveDecision(t *testing.T) {
	owned := map[string]bool{}
	for _, name := range postgresmigrations.OwnedTables {
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
