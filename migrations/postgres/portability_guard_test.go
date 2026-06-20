package postgresmigrations

import (
	"io/fs"
	"regexp"
	"testing"
)

// TestPortabilityInvariant guards the embedded↔standalone data-move contract
// (#544): the OpenRails core schema must stay 100% portable so it can be dumped
// from one mode and loaded into the other with no per-table surgery. Two rules
// the OpenRails migrations must never violate:
//
//  1. No core table may reference the standalone-only AuthKit `profiles` schema.
//     A cross-schema FK would break a standalone→embedded move (where `profiles`
//     is absent) and couple portable billing data to auth state. The merchant↔org
//     link is `merchants.owner_org_id`, deliberately a bare `text` column with NO
//     FK (#544/#541) — that is the ONLY permitted cross-mode reference.
//
//  2. River job-queue tables (`river_*`) must never be created by these
//     migrations. River is runtime/infra state, lives in `public` (#545,
//     config.RiverSchema), and is migrated by rivermigrate — never authored here,
//     so it can never leak into the portable billing schema.
//
// If this test fails, a new migration violated the portability contract; fix the
// migration rather than the test.
func TestPortabilityInvariant(t *testing.T) {
	files, err := fs.Glob(FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no migration files found via embedded FS")
	}

	// Cross-schema FK into the auth schema(s). `profiles` is AuthKit's schema;
	// `authkit` is the migratekit tracking app/schema name — neither is portable.
	authFKRe := regexp.MustCompile(`(?i)\breferences\s+(profiles|authkit)\.`)
	// Any River table creation in an OpenRails migration (qualified or not).
	riverTableRe := regexp.MustCompile(`(?i)\bcreate\s+table\s+(if\s+not\s+exists\s+)?([a-z_]+\.)?river_`)

	for _, name := range files {
		content, rerr := fs.ReadFile(FS, name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		sql := string(content)

		if loc := authFKRe.FindString(sql); loc != "" {
			t.Errorf("%s: foreign key into the auth schema (%q) — core billing tables must not reference `profiles`/`authkit` (#544). The only cross-mode link is merchants.owner_org_id (text, no FK).", name, loc)
		}
		if loc := riverTableRe.FindString(sql); loc != "" {
			t.Errorf("%s: creates a River table (%q) — River lives in `public` and is managed by rivermigrate, never authored in OpenRails migrations (#545).", name, loc)
		}
	}
}
