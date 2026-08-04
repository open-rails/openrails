package queryguard

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// or#858 soft-delete leak guard.
//
// A prune stopped hard-deleting: it sets deleted_at. That trades a loud failure
// (the row is gone) for a silent one (the row is still there and every read that
// forgot the predicate keeps serving it) — a pruned subscription still granting
// access, a pruned payment still counted as revenue. So the predicate is not a
// convention, it is enforced: every generated query that READS or MUTATES a
// soft-deletable table must carry `deleted_at IS NULL` for that table, or name
// itself in the allowlist below with a reason.
//
// Both halves are derived, never hardcoded: the table set comes from the
// migrations, the queries from the generated text the pool actually executes.
// A new query, or a new soft-deletable table, is covered the day it lands.

const (
	migrationsDir = "../../../migrations/postgres"
	genDir        = "../gen"
)

// softDeleteTables the guard polices. Derived from the schema and then
// intersected with this set, because not every deleted_at means the same thing:
//   - merchants.deleted_at is DIRECTORY state (#721) — platform operator reads
//     list tombstoned merchants on purpose. Different feature, different rules.
var policedTables = map[string]string{
	"subscriptions":     "or#858",
	"payments":          "or#858",
	"checkout_sessions": "or#858",
	"entitlements":      "pre-existing soft delete (revocation) + or#858 prune",
}

// allow lists queries that legitimately see soft-deleted rows. Every entry is a
// deliberate decision with a reason; adding one is the review point.
var allow = map[string]string{
	// The prune's own reversal path: it exists to find stamped rows.
	"RestoreSubscriptionsByDestructiveRun":    "the rollback — its whole job is to find soft-deleted rows",
	"RestorePaymentsByDestructiveRun":         "the rollback",
	"RestoreCheckoutSessionsByDestructiveRun": "the rollback",
	"RestoreEntitlementsByDestructiveRun":     "the rollback",

	// Merchant purge (#225) removes EVERY row of a merchant, tombstoned or not.
	// Filtering here would strand soft-deleted rows after the merchant is gone.
	"CountMerchantRowsSubscriptions":    "merchant purge counts every row, including tombstoned",
	"PurgeMerchantRowsSubscriptions":    "merchant purge removes every row, including tombstoned",
	"CountMerchantRowsPayments":         "merchant purge counts every row, including tombstoned",
	"PurgeMerchantRowsPayments":         "merchant purge removes every row, including tombstoned",
	"CountMerchantRowsCheckoutSessions": "merchant purge counts every row, including tombstoned",
	"PurgeMerchantRowsCheckoutSessions": "merchant purge removes every row, including tombstoned",
	"CountMerchantRowsEntitlements":     "merchant purge counts every row, including tombstoned",
	"PurgeMerchantRowsEntitlements":     "merchant purge removes every row, including tombstoned",
}

func TestSoftDeleteTablesAreFilteredEverywhere(t *testing.T) {
	tables := derivePoliced(t)
	if len(tables) != len(policedTables) {
		t.Fatalf("derived %v from the migrations, expected all of %v — schema parsing is broken and the guard would pass vacuously",
			keys(tables), policedNames())
	}

	queries := loadQueries(t)
	if len(queries) < 400 {
		t.Fatalf("loaded only %d generated queries — query parsing is broken, the guard would pass vacuously", len(queries))
	}

	var leaks []string
	checked := 0
	for _, q := range queries {
		for table := range tables {
			for _, ref := range references(q.SQL, table) {
				if ref.insertTarget {
					continue // an INSERT cannot see a row
				}
				checked++
				if hasPredicate(q.SQL, ref.alias) {
					continue
				}
				if _, ok := allow[q.Name]; ok {
					continue
				}
				leaks = append(leaks, fmt.Sprintf("%s (%s): reads openrails.%s as %q with no `%sdeleted_at IS NULL`",
					q.Name, q.File, table, refName(ref.alias), qualifier(ref.alias)))
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d table references checked — reference detection is broken", checked)
	}
	sort.Strings(leaks)
	for _, l := range leaks {
		t.Errorf("soft-delete leak: %s", l)
	}
	if len(leaks) > 0 {
		t.Logf("A soft-deleted row that reaches a live read is worse than a hard delete: it is wrong and silent. " +
			"Add the predicate, or add the query to `allow` in this file with the reason it must see tombstoned rows.")
	}
	t.Logf("or#858 guard: %d references across %d queries carry the soft-delete predicate (%d reviewed exemptions)",
		checked, len(queries), len(allow))
}

// derivePoliced reads the migrations for tables that actually carry deleted_at,
// intersected with the policed set — so the guard tracks the schema and a
// policed table that loses its column fails here rather than passing silently.
func derivePoliced(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files)
	create := regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?openrails\.([a-z_]+) \((.*?)\n\);`)
	alter := regexp.MustCompile(`(?is)ALTER TABLE (?:ONLY )?openrails\.([a-z_]+)\s+(.*?);`)
	out := map[string]bool{}
	for _, f := range files {
		b, rerr := os.ReadFile(f) // #nosec G304 -- test-local glob over the repo's own migrations
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		sql := string(b)
		for _, m := range create.FindAllStringSubmatch(sql, -1) {
			if strings.Contains(m[2], "deleted_at ") {
				out[m[1]] = true
			}
		}
		for _, m := range alter.FindAllStringSubmatch(sql, -1) {
			if strings.Contains(m[2], "ADD COLUMN deleted_at") {
				out[m[1]] = true
			}
			if strings.Contains(m[2], "DROP COLUMN deleted_at") {
				delete(out, m[1])
			}
		}
	}
	policed := map[string]bool{}
	for tbl := range out {
		if _, ok := policedTables[tbl]; ok {
			policed[tbl] = true
		}
	}
	return policed
}

type query struct{ Name, File, SQL string }

var constStart = regexp.MustCompile("^const [a-zA-Z0-9_]+ = `(-- name: ([A-Za-z0-9_]+) :([a-z]+))$")

// loadQueries reads the generated text, not the .sql sources: that is the exact
// string the pool executes, with sqlc.arg/narg/embed already resolved.
func loadQueries(t *testing.T) []query {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(genDir, "*.sql.go"))
	if err != nil {
		t.Fatalf("glob gen: %v", err)
	}
	sort.Strings(files)
	var out []query
	for _, f := range files {
		b, rerr := os.ReadFile(f) // #nosec G304 -- test-local glob over the repo's own generated code
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		lines := strings.Split(string(b), "\n")
		for i := 0; i < len(lines); i++ {
			m := constStart.FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			var body []string
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(lines[j], "`") {
					i = j
					break
				}
				body = append(body, lines[j])
			}
			out = append(out, query{Name: m[2], File: filepath.Base(f), SQL: strings.Join(body, "\n")})
		}
	}
	return out
}

type tableRef struct {
	alias        string // "" = unaliased
	insertTarget bool
}

// aliasStop is the set of words that follow a table name but are NOT an alias.
var aliasStop = map[string]bool{
	"where": true, "set": true, "on": true, "join": true, "left": true, "right": true,
	"inner": true, "full": true, "cross": true, "group": true, "order": true, "limit": true,
	"offset": true, "values": true, "using": true, "returning": true, "select": true,
	"union": true, "having": true, "and": true, "or": true, "from": true, "as": true,
	"for": true, "with": true, "window": true, "except": true, "intersect": true,
}

func references(sql, table string) []tableRef {
	re := regexp.MustCompile(`(?i)(insert\s+into\s+|from\s+|join\s+|update\s+|delete\s+from\s+)openrails\.` + table + `\b([ \t]+(?:as[ \t]+)?([a-z][a-z0-9_]*))?`)
	var out []tableRef
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		ref := tableRef{insertTarget: strings.EqualFold(strings.TrimSpace(m[1]), "insert into")}
		if alias := strings.TrimSpace(m[3]); alias != "" && !aliasStop[strings.ToLower(alias)] {
			ref.alias = alias
		}
		out = append(out, ref)
	}
	return out
}

func hasPredicate(sql, alias string) bool {
	pat := `(?i)\b` + regexp.QuoteMeta(qualifier(alias)) + `deleted_at\s+IS\s+NULL\b`
	if alias == "" {
		// An unaliased reference is satisfied by a bare predicate only.
		pat = `(?i)(^|[^.\w])deleted_at\s+IS\s+NULL\b`
	}
	return regexp.MustCompile(pat).MatchString(sql)
}

func qualifier(alias string) string {
	if alias == "" {
		return ""
	}
	return alias + "."
}

func refName(alias string) string {
	if alias == "" {
		return "(unaliased)"
	}
	return alias
}

func policedNames() []string {
	out := make([]string, 0, len(policedTables))
	for k := range policedTables {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSoftDeleteUniquesExcludeTombstones is the other half of the invariant.
//
// A hard DELETE freed the unique key; a soft delete does not. If a UNIQUE index
// on a soft-deletable table does not exclude tombstoned rows, a subscription
// pruned by a bad snapshot and then re-listed by the provider fails to re-insert
// with a duplicate-key error naming a row nobody can see — the prune becomes
// irreversible in practice even though the rollback exists.
//
// So: every unique index on a policed table must carry `deleted_at IS NULL` in
// its predicate. Derived from the migrations, applied in file order, so a later
// migration that recreates one of these indexes without the predicate fails here
// instead of silently reopening the hole.
func TestSoftDeleteUniquesExcludeTombstones(t *testing.T) {
	policed := derivePoliced(t)
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files)

	// name -> definition. Statements are applied in file order, then in
	// positional order within each file, so a later DROP/RENAME/CREATE wins
	// exactly as it does in Postgres.
	stmt := regexp.MustCompile(`(?is)(CREATE UNIQUE INDEX (?:IF NOT EXISTS )?([a-z_0-9]+)\s+ON openrails\.([a-z_]+)(.*?);)|(DROP INDEX (?:IF EXISTS )?openrails\.([a-z_0-9]+))|(ALTER INDEX openrails\.([a-z_0-9]+) RENAME TO ([a-z_0-9]+))`)
	type ix struct{ table, def, file string }
	live := map[string]ix{}
	for _, f := range files {
		b, rerr := os.ReadFile(f) // #nosec G304 -- test-local glob over the repo's own migrations
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		for _, m := range stmt.FindAllStringSubmatch(string(b), -1) {
			switch {
			case m[1] != "":
				live[m[2]] = ix{table: m[3], def: m[4], file: filepath.Base(f)}
			case m[5] != "":
				delete(live, m[6])
			case m[7] != "":
				if cur, ok := live[m[8]]; ok {
					delete(live, m[8])
					live[m[9]] = cur
				}
			}
		}
	}
	if len(live) < 20 {
		t.Fatalf("parsed only %d unique indexes — index parsing is broken, the guard would pass vacuously", len(live))
	}

	checked := 0
	names := make([]string, 0, len(live))
	for name := range live {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		got := live[name]
		if !policed[got.table] {
			continue
		}
		checked++
		if regexp.MustCompile(`(?i)deleted_at\s+IS\s+NULL`).MatchString(got.def) {
			continue
		}
		t.Errorf("unique index %s on openrails.%s (%s) does not exclude soft-deleted rows: add `deleted_at IS NULL` to its WHERE. "+
			"Without it a pruned row keeps its key, so restoring it — or re-importing what the provider re-created — fails on a duplicate nobody can see (or#858).",
			name, got.table, got.file)
	}
	if checked < 4 {
		t.Fatalf("only %d unique indexes on policed tables checked — expected several", checked)
	}
	t.Logf("or#858 guard: %d unique indexes on soft-deletable tables exclude tombstones", checked)
}
