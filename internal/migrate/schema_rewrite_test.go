package migrate

import (
	"strings"
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
		in := mig("CREATE TABLE openrails.merchants (id uuid);")
		out, err := rewriteMigrationsSchema(in, config.DefaultSchema)
		if err != nil {
			t.Fatal(err)
		}
		if out[0].Content != in[0].Content {
			t.Fatalf("default rewrite changed DDL: %q", out[0].Content)
		}
	})

	t.Run("empty schema is identity", func(t *testing.T) {
		in := mig("CREATE TABLE openrails.merchants (id uuid);")
		out, err := rewriteMigrationsSchema(in, "")
		if err != nil {
			t.Fatal(err)
		}
		if out[0].Content != in[0].Content {
			t.Fatalf("empty rewrite changed DDL: %q", out[0].Content)
		}
	})

	t.Run("custom schema relocates qualifiers and bare schema DDL", func(t *testing.T) {
		in := mig("CREATE SCHEMA IF NOT EXISTS openrails;\n" +
			"CREATE TABLE openrails.merchants (id uuid REFERENCES openrails.merchants);\n" +
			"GRANT USAGE ON SCHEMA openrails TO openrails_app;\n" +
			"ALTER DEFAULT PRIVILEGES IN SCHEMA openrails GRANT SELECT ON TABLES TO openrails_app;")
		out, err := rewriteMigrationsSchema(in, "shop")
		if err != nil {
			t.Fatal(err)
		}
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
		out, err := rewriteMigrationsSchema(in, "shop")
		if err != nil {
			t.Fatal(err)
		}
		if out[0].Content != in[0].Content {
			t.Fatalf("rewrite touched role/prose: %q", out[0].Content)
		}
	})

}

func TestSchemaRelocationPreservesDomainValues(t *testing.T) {
	input := `-- openrails stays documentation
/* outer openrails /* nested openrails */ comment */
CREATE TABLE "openrails".payment_methods (rebill_driver text CHECK (rebill_driver IN ('openrails','provider')), note text DEFAULT 'openrails.payment_methods');
CREATE FUNCTION openrails.sample() RETURNS text SET search_path TO 'pg_catalog', 'openrails' LANGUAGE plpgsql AS $body$
BEGIN
 PERFORM 'openrails.payment_methods'::regclass;
 PERFORM 1 FROM openrails.payment_methods;
 RETURN 'openrails';
END
$body$;`
	got, err := relocateSchemaSQL(input, "shop")
	if err != nil {
		t.Fatal(err)
	}
	for _, unchanged := range []string{"-- openrails stays documentation", "/* outer openrails /* nested openrails */ comment */", "IN ('openrails','provider')", "DEFAULT 'openrails.payment_methods'", "RETURN 'openrails'"} {
		if !strings.Contains(got, unchanged) {
			t.Fatalf("lost literal/comment %q: %s", unchanged, got)
		}
	}
	for _, changed := range []string{"CREATE TABLE shop.payment_methods", "CREATE FUNCTION shop.sample", "'pg_catalog', 'shop'", "'shop.payment_methods'::regclass", "FROM shop.payment_methods"} {
		if !strings.Contains(got, changed) {
			t.Fatalf("missing schema reference %q: %s", changed, got)
		}
	}
}

func TestSchemaRelocationQuotedAndMalformedTokens(t *testing.T) {
	for _, input := range []string{
		"SELECT $$FROM openrails.payment_methods$$, $tag$openrails$tag$;",
		`SELECT 'it''s openrails', E'it\'s openrails', "not openrails";`,
	} {
		got, err := relocateSchemaSQL(input, "shop")
		if err != nil {
			t.Fatal(err)
		}
		if got != input {
			t.Fatalf("quoted data changed: %q", got)
		}
	}
	for _, input := range []string{"SELECT 'unfinished", "SELECT \"unfinished", "/* outer /* inner */", "DO $body$BEGIN"} {
		if _, err := relocateSchemaSQL(input, "shop"); err == nil {
			t.Fatalf("accepted unterminated SQL: %q", input)
		}
	}
}
