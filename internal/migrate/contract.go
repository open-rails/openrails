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
// changes what an older build may use is a contract migration: its name ends
// in contractSuffix, and it ships a release after no code uses what it
// removes. Builds only warn about newer migrations: the release before a
// contract is meant to keep running on it.
const contractSuffix = "_contract.up.sql"

// firstChecked is the first migration the naming rule covers; earlier ones
// predate it.
const firstChecked = 110

var (
	sqlNoise    = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/|'(?:[^']|'')*'`)
	breakingSQL = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|COLUMN|VIEW|MATERIALIZED\s+VIEW|TYPE|SEQUENCE)|ALTER\s+(TABLE|TYPE)\s+(IF\s+EXISTS\s+)?\S+\s+RENAME|ALTER\s+COLUMN\s+\S+\s+(SET\s+DATA\s+)?TYPE|SET\s+NOT\s+NULL)\b`)
)

// checkContracts refuses a migration that may break earlier builds without
// being named a contract migration.
func checkContracts(migrations []migratekit.Migration) error {
	for _, m := range migrations {
		n, err := strconv.Atoi(migratekit.Prefix(m.Name))
		if err != nil || n < firstChecked || !strings.HasSuffix(m.Name, ".up.sql") || strings.HasSuffix(m.Name, contractSuffix) {
			continue
		}
		if hit := breakingSQL.FindString(sqlNoise.ReplaceAllString(m.Content, " ")); hit != "" {
			return fmt.Errorf("openrails: migration %s may break earlier builds (%q): name it *%s and ship it a release after no code uses what it changes", m.Name, hit, contractSuffix)
		}
	}
	return nil
}

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
