package postgresmigrations

import (
	"fmt"
	"strings"
)

// BaselineName is the single consolidated schema migration. Everything the
// schema is, it is because of this file; there is no migration history behind
// it (or#893).
const BaselineName = "0001_schema.up.sql"

// Baseline returns the raw SQL of the consolidated baseline.
func Baseline() (string, error) {
	b, err := FS.ReadFile(BaselineName)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", BaselineName, err)
	}
	return string(b), nil
}

// BaselineObjects returns the baseline DDL for the named objects, in baseline
// order. A function yields its CREATE, its COMMENT and its REVOKE/GRANT; a
// table yields its CREATE and everything grouped with it (comments,
// constraints, indexes, triggers, RLS, policy, grants).
//
// It exists so a harness that stands up a PARTIAL schema — one that cannot
// apply the whole baseline because it never created the sibling `profiles`
// schema, or because its fixtures predate the real table shapes — still
// replays the REAL definition of the objects it depends on. A hand-copied
// SECURITY DEFINER body is exactly the divergence #824 hid behind.
func BaselineObjects(names ...string) (string, error) {
	sql, err := Baseline()
	if err != nil {
		return "", err
	}
	lines := strings.Split(sql, "\n")

	// Block starts: the line index of every CREATE TABLE / CREATE FUNCTION and
	// every section header, so a block ends where the next one begins.
	isBoundary := func(l string) bool {
		return strings.HasPrefix(l, "CREATE TABLE openrails.") ||
			strings.HasPrefix(l, "CREATE FUNCTION openrails.") ||
			strings.HasPrefix(l, "CREATE VIEW openrails.") ||
			strings.HasPrefix(l, "CREATE TYPE openrails.") ||
			strings.HasPrefix(l, "SET default_tablespace") ||
			strings.HasPrefix(l, "-- ---")
	}

	var out []string
	for _, name := range names {
		wantTable := "CREATE TABLE openrails." + name + " ("
		wantFunc := "CREATE FUNCTION openrails." + name + "("
		start := -1
		for i, l := range lines {
			if strings.HasPrefix(l, wantTable) || strings.HasPrefix(l, wantFunc) {
				start = i
				break
			}
		}
		if start < 0 {
			return "", fmt.Errorf("baseline defines no table or function named %q", name)
		}
		end := len(lines)
		for i := start + 1; i < len(lines); i++ {
			if isBoundary(lines[i]) {
				end = i
				break
			}
		}
		out = append(out, strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n"))
	}
	return strings.Join(out, "\n\n") + "\n", nil
}
