package migrate

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
)

func TestUseSingleNodeClickHouseEnginesRewritesReplicatedEngines(t *testing.T) {
	migrations := []migratekit.Migration{{
		Name: "clickhouse.sql",
		Content: `
CREATE TABLE one ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version) ORDER BY id;
CREATE TABLE two ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}_tenant_scoped_v2', '{replica}', last_updated) ORDER BY id;
CREATE TABLE three ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}') ORDER BY id;
`,
	}}

	got := useSingleNodeClickHouseEngines(migrations)
	content := got[0].Content

	if strings.Contains(content, "ReplicatedReplacingMergeTree") || strings.Contains(content, "{replica}") {
		t.Fatalf("replicated engine was not fully rewritten:\n%s", content)
	}
	for _, want := range []string{"ReplacingMergeTree(version)", "ReplacingMergeTree(last_updated)", "ReplacingMergeTree()"} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q in rewritten migration:\n%s", want, content)
		}
	}
}

func TestPatchAuthKitAPIKeyRoleMigrationBackfillsLegacyRows(t *testing.T) {
	migrations := []migratekit.Migration{{
		Name: "007_api_key_role.up.sql",
		Content: `ALTER TABLE profiles.service_tokens
  ADD COLUMN role text NOT NULL;

ALTER TABLE profiles.service_tokens
  ADD CONSTRAINT service_tokens_role_format_chk CHECK (char_length(role) BETWEEN 1 AND 64);`,
	}}

	got := patchAuthKitAPIKeyRoleMigration(migrations)
	content := got[0].Content

	if strings.Contains(content, "ADD COLUMN role text NOT NULL") {
		t.Fatalf("unsafe role column DDL survived:\n%s", content)
	}
	for _, want := range []string{
		"ADD COLUMN IF NOT EXISTS role text",
		"INSERT INTO profiles.org_roles (org_id, role)",
		"UPDATE profiles.service_tokens",
		"ALTER COLUMN role SET NOT NULL",
		"service_tokens_role_format_chk",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q in patched migration:\n%s", want, content)
		}
	}
}
