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
