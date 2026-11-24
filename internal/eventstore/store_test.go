package eventstore

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMigrationsSplitIntoDatabaseQualifiedStatements(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 || migrations[0].Version != 1 || migrations[0].Name != "0001_usage.sql" {
		t.Fatalf("migrations = %+v", migrations)
	}
	s := &Store{database: "critical_x"}
	created := map[string]bool{}
	for _, stmt := range migrations[0].Statements {
		if strings.Contains(stmt, "--") {
			t.Fatalf("a comment reached a statement: %s", stmt)
		}
		resolved := s.resolve(stmt)
		if strings.Contains(resolved, databaseToken) || !strings.Contains(resolved, "`critical_x`.") {
			t.Fatalf("statement does not name the database: %s", resolved)
		}
		fields := strings.Fields(stmt)
		if len(fields) > 5 && fields[1] == "TABLE" {
			created[fields[5]] = true
		}
	}
	for _, name := range []string{"{database}.usage_events", "{database}.limit_usage", "{database}.usage_quantities", "{database}.usage_minutes", "{database}.usage_history"} {
		if !created[name] {
			t.Errorf("no CREATE TABLE %s in %v", name, created)
		}
	}
}

func TestStatementsDropWholeLineComments(t *testing.T) {
	got := Statements("-- a; b\nCREATE TABLE a (x UInt8) ENGINE = Memory;\n  -- c\n\nSELECT 1;\n")
	want := []string{"CREATE TABLE a (x UInt8) ENGINE = Memory", "SELECT 1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Statements = %q", got)
	}
}

func TestLiteralsAreFormattedValues(t *testing.T) {
	id := uuid.MustParse("01938e4a-0000-7000-8000-000000000001")
	if got := uuidLiteral(id); got != "toUUID('01938e4a-0000-7000-8000-000000000001')" {
		t.Fatal(got)
	}
	at := time.Date(2026, 11, 24, 1, 2, 3, 123456789, time.FixedZone("x", 3600))
	if got := timeLiteral(at); got != "toDateTime64('2026-11-24 00:02:03.123456', 6, 'UTC')" {
		t.Fatal(got)
	}
}
