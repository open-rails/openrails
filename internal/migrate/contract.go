package migrate

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/migratekit"
)

// Hosts sharing one schema roll out one at a time, so an older build keeps
// running on a schema a newer one migrated. A migration that removes or
// changes what an older build may use (a drop, a rename, a type change) is a
// contract migration: its name ends in contractSuffix, and it ships a release
// after no code uses what it removes. An older build that finds one applied
// refuses to run rather than failing on the missing object.
const contractSuffix = "_contract.up.sql"

// firstChecked is the first migration the naming rule covers; earlier ones
// predate it.
const firstChecked = 110

var breakingSQL = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|COLUMN|VIEW|FUNCTION|TYPE|SEQUENCE)|RENAME\s+(TO|COLUMN)|ALTER\s+COLUMN\s+\S+\s+(SET\s+DATA\s+)?TYPE)\b`)

// checkContracts refuses a migration that breaks earlier builds without being
// named a contract migration.
func checkContracts(migrations []migratekit.Migration) error {
	for _, m := range migrations {
		n, err := strconv.Atoi(migratekit.Prefix(m.Name))
		if err != nil || n < firstChecked || !strings.HasSuffix(m.Name, ".up.sql") || strings.HasSuffix(m.Name, contractSuffix) {
			continue
		}
		if hit := breakingSQL.FindString(m.Content); hit != "" {
			return fmt.Errorf("openrails: migration %s breaks earlier builds (%q): name it *%s and ship it a release after no code uses what it removes", m.Name, hit, contractSuffix)
		}
	}
	return nil
}

// newerMigrations names the applied migrations this build does not carry,
// and refuses when one of them is a contract migration.
func newerMigrations(applied []migratekit.AppliedRecord, migrations []migratekit.Migration) ([]string, error) {
	var newer []string
	for _, r := range applied {
		if r.Filename == "" || slices.ContainsFunc(migrations, func(m migratekit.Migration) bool { return m.Name == r.Filename }) {
			continue
		}
		if strings.HasSuffix(r.Filename, contractSuffix) {
			return nil, fmt.Errorf("openrails: the schema has contract migration %s, which this build predates and may depend on what it removed; run a release that carries it", r.Filename)
		}
		newer = append(newer, r.Filename)
	}
	return newer, nil
}
