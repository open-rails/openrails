package migrate

import (
	"testing"

	"github.com/open-rails/migratekit"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/stretchr/testify/require"
)

// Every migration from firstChecked on that breaks earlier builds is named a
// contract migration.
func TestBreakingMigrationsAreContracts(t *testing.T) {
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	require.NoError(t, checkContracts(migrations))
}

func TestCheckContracts(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		ok        bool
	}{
		{"0110_add.up.sql", "ALTER TABLE billing.orders ADD COLUMN note text;", true},
		{"0110_comment.up.sql", "-- replaces the DROP TABLE of 0098\nALTER TABLE billing.orders ADD COLUMN note text; /* no DROP COLUMN */", true},
		{"0110_drop.up.sql", "ALTER TABLE billing.orders DROP COLUMN note;", false},
		{"0110_drop_table.up.sql", "DROP TABLE IF EXISTS billing.document_sequences;", false},
		{"0110_drop_matview.up.sql", "DROP MATERIALIZED VIEW billing.totals;", false},
		{"0110_retype.up.sql", "ALTER TABLE billing.orders ALTER COLUMN number TYPE bigint USING number::bigint;", false},
		{"0110_retype_short.up.sql", "ALTER TABLE billing.orders ALTER number TYPE bigint;", false},
		{"0110_rename.up.sql", "ALTER TABLE billing.orders RENAME COLUMN note TO memo;", false},
		{"0110_rename_short.up.sql", "ALTER TABLE billing.orders RENAME note TO memo;", false},
		{"0110_rename_value.up.sql", "ALTER TYPE billing.state RENAME VALUE 'a' TO 'b';", false},
		{"0110_not_null.up.sql", "ALTER TABLE billing.orders ALTER COLUMN note SET NOT NULL;", false},
		{"0110_drop_default.up.sql", "ALTER TABLE billing.orders ALTER COLUMN note DROP DEFAULT;", false},
		{"0110_drop_constraint.up.sql", "ALTER TABLE billing.orders DROP CONSTRAINT orders_note_key;", false},
		{"0110_drop_index.up.sql", "DROP INDEX billing.orders_note_key;", false},
		{"0110_drop_contract.up.sql", "ALTER TABLE billing.orders DROP COLUMN note;", true},
		{"0098_before_rule.up.sql", "DROP TABLE billing.document_sequences;", true},
		{"0110_drop.down.sql", "DROP TABLE billing.orders_notes;", true},
	} {
		err := checkContracts([]migratekit.Migration{{Name: tc.name, Content: tc.sql}})
		if tc.ok {
			require.NoError(t, err, tc.name)
		} else {
			require.ErrorContains(t, err, tc.name, tc.name)
		}
	}
}

func TestNewerMigrations(t *testing.T) {
	chain := []migratekit.Migration{{Name: "0109_tenant_merchant_binding.up.sql"}}
	contracts, others := newerMigrations([]migratekit.AppliedRecord{{Filename: "0109_tenant_merchant_binding.up.sql"},
		{Filename: "0110_notes.up.sql"}, {Filename: "0111_drop_notes_contract.up.sql"}}, chain)
	require.Equal(t, []string{"0111_drop_notes_contract.up.sql"}, contracts)
	require.Equal(t, []string{"0110_notes.up.sql"}, others)
}
