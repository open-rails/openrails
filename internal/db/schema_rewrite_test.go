package db

import (
	"testing"

	"github.com/open-rails/openrails/config"
)

// TestSchemaRewriter verifies the #471 schema-qualifier rewrite: identity for the
// default schema, relocation for a custom one, and that it only touches the
// `openrails.` qualifier.
func TestSchemaRewriter(t *testing.T) {
	t.Run("default schema is identity", func(t *testing.T) {
		r := newSchemaRewriter(config.DefaultSchema)
		if r.active {
			t.Fatalf("rewriter for default schema must be inactive")
		}
		const sql = "SELECT * FROM openrails.merchants WHERE id = $1"
		if got := r.apply(sql); got != sql {
			t.Fatalf("identity rewrite changed SQL: %q", got)
		}
	})

	t.Run("empty schema is identity", func(t *testing.T) {
		if newSchemaRewriter("").active {
			t.Fatalf("empty schema must be inactive")
		}
	})

	t.Run("custom schema relocates the qualifier", func(t *testing.T) {
		r := newSchemaRewriter("shop")
		if !r.active {
			t.Fatalf("custom schema must be active")
		}
		got := r.apply("INSERT INTO openrails.payments (id) SELECT id FROM openrails.subscriptions")
		want := "INSERT INTO shop.payments (id) SELECT id FROM shop.subscriptions"
		if got != want {
			t.Fatalf("rewrite mismatch:\n got  %q\n want %q", got, want)
		}
	})

	t.Run("does not touch unrelated identifiers", func(t *testing.T) {
		r := newSchemaRewriter("shop")
		// public.* and profiles.* (AuthKit) qualifiers must survive untouched.
		const sql = "SELECT public.gen_random_uuid(), p.id FROM profiles.users p JOIN openrails.customers c ON c.subject = p.id"
		want := "SELECT public.gen_random_uuid(), p.id FROM profiles.users p JOIN shop.customers c ON c.subject = p.id"
		if got := r.apply(sql); got != want {
			t.Fatalf("rewrite touched unrelated schema:\n got  %q\n want %q", got, want)
		}
	})
}
