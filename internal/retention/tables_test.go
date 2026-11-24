package retention_test

import (
	"bufio"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/retention"
)

// TestTablesMatchSchema holds retention.Tables to the schema: the snapshot
// the migrations install lists exactly its tables, and each table's latest
// comment states its rule.
func TestTablesMatchSchema(t *testing.T) {
	snapshot, err := os.Open("../../api/schema.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var schema []string
	lines := bufio.NewScanner(snapshot)
	for lines.Scan() {
		if name, ok := strings.CutPrefix(lines.Text(), "table billing."); ok {
			name, partitioned, _ := strings.Cut(name, " ")
			if partitioned != "" && retention.Tables[name].Class != retention.PartitionedMonthly {
				t.Errorf("partitioned table %s is not classified partitioned", name)
			}
			schema = append(schema, name)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	if len(schema) == 0 {
		t.Fatal("api/schema.txt lists no table")
	}
	for _, name := range schema {
		if _, ok := retention.Tables[name]; !ok {
			t.Errorf("table %s has no retention class in retention.Tables", name)
		}
	}
	for name := range retention.Tables {
		if !slices.Contains(schema, name) {
			t.Errorf("retention.Tables classifies %s, which the schema does not have", name)
		}
	}

	comments := tableComments(t)
	for name, table := range retention.Tables {
		comment, ok := comments[name]
		switch {
		case !ok:
			t.Errorf("table %s has no comment", name)
		case table.Class == retention.State && strings.Contains(comment, "Retention:"):
			t.Errorf("state table %s states a retention rule", name)
		case table.Class != retention.State && !strings.Contains(comment, "Retention: "+table.Rule):
			t.Errorf("table %s's comment does not state its rule %q", name, table.Rule)
		}
	}
}

var tableComment = regexp.MustCompile(`(?s)COMMENT ON TABLE billing\.([a-z0-9_]+) IS '((?:[^']|'')*)'`)

// tableComments is each table's comment as the last migration to set it left
// it.
func tableComments(t *testing.T) map[string]string {
	t.Helper()
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	out := map[string]string{}
	for _, file := range files {
		sql, err := fs.ReadFile(postgresmigrations.FS, file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tableComment.FindAllStringSubmatch(string(sql), -1) {
			out[m[1]] = strings.ReplaceAll(m[2], "''", "'")
		}
	}
	return out
}
