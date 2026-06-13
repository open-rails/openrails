package migrate

import (
	"testing"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
)

// TestRewriteMigrationsSchema verifies the #471 migration-DDL rewrite: identity
// for the default schema, relocation for a custom one.
func TestRewriteMigrationsSchema(t *testing.T) {
	mig := func(content string) []migratekit.Migration {
		return []migratekit.Migration{{Content: content}}
	}

	t.Run("default schema is identity", func(t *testing.T) {
		in := mig("CREATE TABLE openrails.tenants (id uuid);")
		out := rewriteMigrationsSchema(in, config.DefaultSchema)
		if out[0].Content != in[0].Content {
			t.Fatalf("default rewrite changed DDL: %q", out[0].Content)
		}
	})

	t.Run("empty schema is identity", func(t *testing.T) {
		in := mig("CREATE TABLE openrails.tenants (id uuid);")
		out := rewriteMigrationsSchema(in, "")
		if out[0].Content != in[0].Content {
			t.Fatalf("empty rewrite changed DDL: %q", out[0].Content)
		}
	})

	t.Run("custom schema relocates qualifiers and bare schema DDL", func(t *testing.T) {
		in := mig("CREATE SCHEMA IF NOT EXISTS openrails;\n" +
			"CREATE TABLE openrails.tenants (id uuid REFERENCES openrails.tenants);\n" +
			"GRANT USAGE ON SCHEMA openrails TO openrails_app;\n" +
			"ALTER DEFAULT PRIVILEGES IN SCHEMA openrails GRANT SELECT ON TABLES TO openrails_app;")
		out := rewriteMigrationsSchema(in, "shop")
		want := "CREATE SCHEMA IF NOT EXISTS shop;\n" +
			"CREATE TABLE shop.tenants (id uuid REFERENCES shop.tenants);\n" +
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
}
