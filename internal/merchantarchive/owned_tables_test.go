package merchantarchive

import (
	"testing"

	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

func TestEveryOwnedTableHasArchiveDecision(t *testing.T) {
	classified := map[string]bool{}
	for _, p := range contract.Profiles {
		classified[p.Name] = true
	}
	for name := range excludedTables {
		classified[name] = true
	}
	for _, name := range postgresmigrations.OwnedTables {
		if !classified[name] {
			t.Errorf("new OpenRails-owned table %s requires an explicit archive decision", name)
		}
		delete(classified, name)
	}
	for name := range classified {
		t.Errorf("archive claims non-owned table %s", name)
	}
}
