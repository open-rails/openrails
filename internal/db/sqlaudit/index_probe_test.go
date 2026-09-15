//go:build cgo

package sqlaudit

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestIndexAvailabilityProbeRejectsMissingPredicateIndex(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires the disposable SQLC vet database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// The unrelated primary key gives the planner a possible full index scan.
	// Everything is rolled back, including fixture DDL and session settings.
	_, err = conn.Exec(ctx, `BEGIN;
		CREATE TABLE openrails.audit_index_probe (id bigint PRIMARY KEY, merchant_id uuid, needle text);
		CREATE INDEX ON openrails.audit_index_probe(id) WHERE true;
		CREATE INDEX ON openrails.audit_index_probe(id) WHERE id IS NOT NULL;
		GRANT SELECT ON openrails.audit_index_probe TO openrails_app;`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "ROLLBACK") //nolint:errcheck // fixture teardown
	q := Query{Name: "index_probe", Kind: "many", SQL: "SELECT id FROM openrails.audit_index_probe WHERE needle=$1::text LIMIT 1"}
	structure, err := q.Parse()
	if err != nil {
		t.Fatal(err)
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			// A one-row table makes a normal sequential scan cheaper. That cost
			// choice must not conceal the useful index from an availability probe.
			if _, err := conn.Exec(ctx, `RESET ROLE;
				CREATE INDEX ON openrails.audit_index_probe(needle);
				INSERT INTO openrails.audit_index_probe(id,needle) VALUES(1,'present');
				ANALYZE openrails.audit_index_probe;`); err != nil {
				t.Fatal(err)
			}
		}
		catalog, err := LoadCatalog(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		if err := PrepareSession(ctx, conn); err != nil {
			t.Fatal(err)
		}
		plan, err := GenericPlan(ctx, conn, q.SQL)
		if err != nil {
			t.Fatal(err)
		}
		findings := planFindings(q, structure, plan, catalog)
		if indexed && len(findings) != 0 {
			t.Fatalf("usable index was refused: %v", findings)
		}
		if !indexed {
			found := false
			for _, finding := range findings {
				found = found || finding.Rule == RuleSeqScan
			}
			if !found {
				t.Fatalf("missing predicate index was accepted: %v", findings)
			}
		}
	}
}
