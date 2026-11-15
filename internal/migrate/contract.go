package migrate

import (
	"slices"
	"strings"

	"github.com/open-rails/migratekit"
)

// Hosts sharing one schema roll out one at a time, so an older build keeps
// running on a schema a newer one migrated. A migration that removes or
// changes what an older build may use is a contract migration: its name ends
// in contractSuffix, and it ships a release after no code uses what it
// removes. Builds only warn about newer migrations: the release before a
// contract is meant to keep running on it.
const contractSuffix = "_contract.up.sql"

// newerMigrations names the applied migrations this build does not carry,
// contract ones first.
func newerMigrations(applied []migratekit.AppliedRecord, migrations []migratekit.Migration) (contracts, others []string) {
	for _, r := range applied {
		if r.Filename == "" || slices.ContainsFunc(migrations, func(m migratekit.Migration) bool { return m.Name == r.Filename }) {
			continue
		}
		if strings.HasSuffix(r.Filename, contractSuffix) {
			contracts = append(contracts, r.Filename)
		} else {
			others = append(others, r.Filename)
		}
	}
	return contracts, others
}
