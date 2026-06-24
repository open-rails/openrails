package migrate

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

// TestRewriteMigrationsSchema verifies the #471 migration-DDL rewrite: identity
// for the default schema, relocation for a custom one.
func TestRewriteMigrationsSchema(t *testing.T) {
	mig := func(content string) []migratekit.Migration {
		return []migratekit.Migration{{Content: content}}
	}

	t.Run("default schema is identity", func(t *testing.T) {
		in := mig("CREATE TABLE openrails.merchants (id uuid);")
		out := rewriteMigrationsSchema(in, config.DefaultSchema)
		if out[0].Content != in[0].Content {
			t.Fatalf("default rewrite changed DDL: %q", out[0].Content)
		}
	})

	t.Run("empty schema is identity", func(t *testing.T) {
		in := mig("CREATE TABLE openrails.merchants (id uuid);")
		out := rewriteMigrationsSchema(in, "")
		if out[0].Content != in[0].Content {
			t.Fatalf("empty rewrite changed DDL: %q", out[0].Content)
		}
	})

	t.Run("custom schema relocates qualifiers and bare schema DDL", func(t *testing.T) {
		in := mig("CREATE SCHEMA IF NOT EXISTS openrails;\n" +
			"CREATE TABLE openrails.merchants (id uuid REFERENCES openrails.merchants);\n" +
			"GRANT USAGE ON SCHEMA openrails TO openrails_app;\n" +
			"ALTER DEFAULT PRIVILEGES IN SCHEMA openrails GRANT SELECT ON TABLES TO openrails_app;")
		out := rewriteMigrationsSchema(in, "shop")
		want := "CREATE SCHEMA IF NOT EXISTS shop;\n" +
			"CREATE TABLE shop.merchants (id uuid REFERENCES shop.merchants);\n" +
			"GRANT USAGE ON SCHEMA shop TO openrails_app;\n" +
			"ALTER DEFAULT PRIVILEGES IN SCHEMA shop GRANT SELECT ON TABLES TO openrails_app;"
		if out[0].Content != want {
			t.Fatalf("custom rewrite mismatch:\n got  %q\n want %q", out[0].Content, want)
		}
	})

	t.Run("leaves the openrails_app role and prose untouched", func(t *testing.T) {
		in := mig("-- OpenRails billing schema (billing-namespace prose)\nCREATE ROLE openrails_app NOLOGIN;")
		out := rewriteMigrationsSchema(in, "shop")
		if out[0].Content != in[0].Content {
			t.Fatalf("rewrite touched role/prose: %q", out[0].Content)
		}
	})

	t.Run("relocates reconciliation status migration", func(t *testing.T) {
		b, err := postgresmigrations.FS.ReadFile("025_reconciliation_status_workflow_terms.up.sql")
		if err != nil {
			t.Fatalf("read migration 025: %v", err)
		}
		out := rewriteMigrationsSchema(mig(string(b)), "shop")[0].Content
		for _, forbidden := range []string{
			"openrails.reconciliation_findings",
			"openrails.idx_reconciliation_findings_open",
			"openrails.idx_reconciliation_findings_admin_queue",
		} {
			if strings.Contains(out, forbidden) {
				t.Fatalf("migration 025 still contains %q after rewrite:\n%s", forbidden, out)
			}
		}
		for _, want := range []string{
			"ALTER TABLE shop.reconciliation_findings",
			"UPDATE shop.reconciliation_findings",
			"DROP INDEX IF EXISTS shop.idx_reconciliation_findings_open",
			"ON shop.reconciliation_findings",
			"COMMENT ON TABLE shop.reconciliation_findings",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("migration 025 rewrite missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("relocates reconciliation findings simplification migration", func(t *testing.T) {
		b, err := postgresmigrations.FS.ReadFile("026_simplify_reconciliation_findings.up.sql")
		if err != nil {
			t.Fatalf("read migration 026: %v", err)
		}
		out := rewriteMigrationsSchema(mig(string(b)), "shop")[0].Content
		for _, forbidden := range []string{
			"openrails.reconciliation_findings",
			"openrails.uq_reconciliation_findings_identity",
			"openrails.idx_reconciliation_findings_actionable",
			"openrails.idx_reconciliation_findings_requires_review",
		} {
			if strings.Contains(out, forbidden) {
				t.Fatalf("migration 026 still contains %q after rewrite:\n%s", forbidden, out)
			}
		}
		for _, want := range []string{
			"ALTER TABLE shop.reconciliation_findings",
			"UPDATE shop.reconciliation_findings",
			"DROP INDEX IF EXISTS shop.uq_reconciliation_findings_identity",
			"ON shop.reconciliation_findings",
			"COMMENT ON TABLE shop.reconciliation_findings",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("migration 026 rewrite missing %q:\n%s", want, out)
			}
		}
	})
}
