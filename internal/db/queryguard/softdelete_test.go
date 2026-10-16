package queryguard

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A prune sets deleted_at instead of deleting. A read that forgets the
// predicate keeps serving the row (a pruned subscription still grants access,
// a pruned payment still counts as revenue), so every generated query that
// reads or mutates a policed table must filter tombstones or be allowed below
// with a reason. Tables come from the migrations, queries from the generated
// text; neither is listed by hand.

const (
	migrationsDir = "../../migrate/postgres"
	genDir        = "../gen"
)

// merchants.deleted_at is directory state, deliberately not policed.
var policedTables = []string{"order_attempts", "payments", "product_access", "subscriptions"}

var allow = map[string]string{
	"RefundRevokedAccessWindow":             "a retained refund revocation must still block access restoration after its payment or subscription is tombstoned",
	"DeleteClosedUnpaidOrders":              "a tombstoned attempt still references its order, which retention must then keep",
	"UsageRecoveryHasOtherPayment":          "a tombstoned payment still owns its provider transaction; recovery must not allocate it to another order",
	"GetUsageRecoveryPayment":               "a tombstoned usage order payment still owns its provider transaction; recovery must not allocate it again",
	"GetOrderPayment":                       "a collection's own payment, tombstoned or not, means the charge settled",
	"ListOrderCollectionsForArchive":        "archive validates each collection's payment including tombstones",
	"CountOrderAttempts":                    "a scheduled collection's ordinal counts every attempt ever made, so its key is never reused",
	"GetInitialMembershipForUpdate":         "completion inspects tombstones to preserve later cancellation and never recreate the accepted ID",
	"GetInitialMembershipForArchive":        "archive validates retained membership identity including later tombstones",
	"ListObservedInitialMembershipPayments": "archive validates observed first-payment history including tombstones",
	"ProposalSubjectIsLocal":                "a tombstoned subscription still names the provider schedule, which therefore is not foreign",
	"PSPForeignObjectCheck":                 "a tombstoned subscription or payment still names the provider object, which therefore is not foreign",
	"MerchantHasActivity":                   "retirement is only for never-used merchants; tombstoned rows disqualify",
	"ProductHoldsReferences":                "a tombstoned subscription or access still referred to the product, which therefore is never deleted",
	"PricesHoldReferences":                  "a tombstoned payment, subscription or order line still referred to the price, which therefore is never deleted",
	"ListHostEvents":                        "a settled event stays deliverable after its payment is tombstoned",
	"ListHostEventsByIDs":                   "a settled event stays deliverable after its payment is tombstoned",
	"RestoreSubscriptionsByDestructiveRun":  "prune rollback: finds soft-deleted rows",
	"RestorePaymentsByDestructiveRun":       "prune rollback",
	"RestoreOrderAttemptsByDestructiveRun":  "prune rollback",
	"RestoreAccessByDestructiveRun":         "prune rollback",
	"CountPruneRestorableForRun":            "undo dry run counts exactly the rows the rollback would restore",
	"CountMerchantRowsSubscriptions":        "merchant purge covers every row",
	"PurgeMerchantRowsSubscriptions":        "merchant purge covers every row",
	"CountMerchantRowsPayments":             "merchant purge covers every row",
	"PurgeMerchantRowsPayments":             "merchant purge covers every row",
	"CountMerchantRowsOrderAttempts":        "merchant purge covers every row",
	"PurgeMerchantRowsOrderAttempts":        "merchant purge covers every row",
	"CountMerchantRowsProductAccess":        "merchant purge covers every row",
	"PurgeMerchantRowsProductAccess":        "merchant purge covers every row",
}

func TestSoftDeleteTablesAreFilteredEverywhere(t *testing.T) {
	tables := softDeleteTables(t)
	for _, table := range policedTables {
		if !tables[table] {
			t.Fatalf("policed table %s has no deleted_at in the migrations; schema parsing is broken or the column was dropped", table)
		}
	}
	queries := loadQueries(t)
	if len(queries) < 400 {
		t.Fatalf("parsed only %d generated queries; the guard would pass vacuously", len(queries))
	}
	checked, needed := 0, map[string]bool{}
	for _, q := range queries {
		for _, table := range policedTables {
			for _, alias := range readers(q.sql, table) {
				checked++
				if hasPredicate(q.sql, alias) {
					continue
				}
				if _, ok := allow[q.name]; ok {
					needed[q.name] = true
					continue
				}
				t.Errorf("soft-delete leak: %s (%s) reads billing.%s as %q without `deleted_at IS NULL`; add the predicate or an allow entry with its reason",
					q.name, q.file, table, alias)
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d references checked; reference detection is broken", checked)
	}
	for name := range allow {
		if !needed[name] {
			t.Errorf("allow entry %s no longer exempts anything; remove it", name)
		}
	}
}

// Soft delete does not free a unique key. A unique index that counts tombstones
// makes a restore, or re-importing what the provider re-created, fail on a
// duplicate nobody can see.
func TestSoftDeleteUniquesExcludeTombstones(t *testing.T) {
	stmt := regexp.MustCompile(`(?is)(CREATE UNIQUE INDEX (?:IF NOT EXISTS )?([a-z_0-9]+)\s+ON billing\.([a-z_]+)(.*?);)|(DROP INDEX (?:IF EXISTS )?billing\.([a-z_0-9]+))|(ALTER INDEX billing\.([a-z_0-9]+) RENAME TO ([a-z_0-9]+))`)
	constraint := regexp.MustCompile(`(?is)ALTER TABLE (?:ONLY )?billing\.([a-z_]+)\s+ADD CONSTRAINT ([a-z_0-9]+) UNIQUE \(([^)]*)\)`)
	type index struct{ table, def string }
	live := map[string]index{}
	policed := map[string]bool{}
	for _, table := range policedTables {
		policed[table] = true
	}
	for _, sql := range migrations(t) {
		// Statements apply in file then positional order, as in Postgres.
		for _, m := range stmt.FindAllStringSubmatch(sql, -1) {
			switch {
			case m[1] != "":
				live[m[2]] = index{m[3], m[4]}
			case m[5] != "":
				delete(live, m[6])
			case m[7] != "":
				if cur, ok := live[m[8]]; ok {
					delete(live, m[8])
					live[m[9]] = cur
				}
			}
		}
		// A table constraint cannot carry a predicate; it is safe only as a
		// superkey of the primary key (a composite FK target).
		for _, m := range constraint.FindAllStringSubmatch(sql, -1) {
			if policed[m[1]] && !containsColumn(m[3], "id") {
				t.Errorf("unique constraint %s on billing.%s (%s) counts tombstones; use a partial unique index with deleted_at IS NULL", m[2], m[1], m[3])
			}
		}
	}
	if len(live) < 20 {
		t.Fatalf("parsed only %d unique indexes; the guard would pass vacuously", len(live))
	}
	checked := 0
	for name, ix := range live {
		if !policed[ix.table] {
			continue
		}
		checked++
		if !regexp.MustCompile(`(?i)deleted_at\s+IS\s+NULL`).MatchString(ix.def) {
			t.Errorf("unique index %s on billing.%s does not exclude soft-deleted rows", name, ix.table)
		}
	}
	if checked < 4 {
		t.Fatalf("only %d unique indexes on policed tables checked", checked)
	}
}

func migrations(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files)
	out := make([]string, len(files))
	for i, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- the repo's own migrations
		if err != nil {
			t.Fatal(err)
		}
		out[i] = string(b)
	}
	return out
}

func softDeleteTables(t *testing.T) map[string]bool {
	create := regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?billing\.([a-z_]+) \((.*?)\n\);`)
	alter := regexp.MustCompile(`(?is)ALTER TABLE (?:ONLY )?billing\.([a-z_]+)\s+(.*?);`)
	out := map[string]bool{}
	for _, sql := range migrations(t) {
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
			if renamed, ok := strings.CutPrefix(m[2], "RENAME TO "); ok && out[m[1]] {
				delete(out, m[1])
				out[strings.TrimSpace(renamed)] = true
			}
		}
	}
	return out
}

type query struct{ name, file, sql string }

// The generated text is what the pool executes, with sqlc macros resolved.
func loadQueries(t *testing.T) []query {
	t.Helper()
	start := regexp.MustCompile("^const [a-zA-Z0-9_]+ = `-- name: ([A-Za-z0-9_]+) :[a-z]+$")
	files, err := filepath.Glob(filepath.Join(genDir, "*.sql.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []query
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- the repo's own generated code
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i := 0; i < len(lines); i++ {
			m := start.FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(lines[j], "`") {
				j++
			}
			out = append(out, query{m[1], filepath.Base(f), strings.Join(lines[i+1:j], "\n")})
			i = j
		}
	}
	return out
}

// Words that may follow a table name without being its alias.
var notAlias = map[string]bool{
	"where": true, "set": true, "on": true, "join": true, "left": true, "right": true,
	"inner": true, "full": true, "cross": true, "group": true, "order": true, "limit": true,
	"offset": true, "values": true, "using": true, "returning": true, "select": true,
	"union": true, "having": true, "and": true, "or": true, "from": true, "as": true,
	"for": true, "with": true, "window": true, "except": true, "intersect": true,
}

// readers returns the alias ("" when unaliased) of every non-INSERT reference.
func readers(sql, table string) []string {
	re := regexp.MustCompile(`(?i)(insert\s+into\s+|from\s+|join\s+|update\s+|delete\s+from\s+)billing\.` + table + `\b([ \t]+(?:as[ \t]+)?([a-z][a-z0-9_]*))?`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		if strings.EqualFold(strings.Join(strings.Fields(m[1]), " "), "insert into") {
			continue // an INSERT cannot see a row
		}
		alias := m[3]
		if notAlias[strings.ToLower(alias)] {
			alias = ""
		}
		out = append(out, alias)
	}
	return out
}

func hasPredicate(sql, alias string) bool {
	pat := `(?i)(^|[^.\w])deleted_at\s+IS\s+NULL\b` // unaliased: a bare predicate only
	if alias != "" {
		pat = `(?i)\b` + regexp.QuoteMeta(alias+".") + `deleted_at\s+IS\s+NULL\b`
	}
	return regexp.MustCompile(pat).MatchString(sql)
}

func containsColumn(list, column string) bool {
	for _, c := range strings.Split(list, ",") {
		if strings.TrimSpace(c) == column {
			return true
		}
	}
	return false
}

func TestGuardParsersRecognizeTheShapesTheyPolice(t *testing.T) {
	for sql, want := range map[string][]string{
		"SELECT 1 FROM billing.payments p WHERE p.deleted_at IS NULL":          {"p"},
		"SELECT 1 FROM billing.payments WHERE deleted_at IS NULL":              {""},
		"UPDATE billing.payments SET x = 1 WHERE id = $1":                      {""},
		"INSERT INTO billing.payments (id) VALUES ($1)":                        nil,
		"SELECT 1 FROM billing.payments AS pay JOIN billing.payments_x q ON 1": {"pay"},
	} {
		if got := readers(sql, "payments"); strings.Join(got, ",") != strings.Join(want, ",") || len(got) != len(want) {
			t.Errorf("readers(%q) = %q, want %q", sql, got, want)
		}
	}
	for sql, want := range map[string]bool{
		"WHERE p.deleted_at IS NULL":      true,
		"WHERE deleted_at IS NULL":        false, // may belong to another table
		"WHERE q.deleted_at IS NULL":      false,
		"WHERE p.deleted_at IS NOT NULL":  false,
		"WHERE p.deleted_at_hint IS NULL": false,
	} {
		if got := hasPredicate(sql, "p"); got != want {
			t.Errorf("hasPredicate(%q, p) = %v", sql, got)
		}
	}
	if hasPredicate("WHERE p.deleted_at IS NULL", "") {
		t.Error("an aliased predicate must not satisfy an unaliased reference")
	}
}
