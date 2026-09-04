//go:build cgo

package sqlaudit

import (
	"os"
	"testing"
)

// testCatalog mirrors the shape the auditor reads from pg_catalog: subscriptions
// is merchant-scoped with an indexed id/customer_id and an unindexed
// cancel_feedback; products is a merchant-scoped catalog table.
func testCatalog() *Catalog {
	return &Catalog{
		MerchantScoped: map[string]bool{"subscriptions": true, "products": true},
		Indexed: map[string]bool{
			"subscriptions.id": true, "subscriptions.merchant_id": true,
			"subscriptions.customer_id": true, "subscriptions.rail": true,
			"products.id": true, "products.merchant_id": true,
		},
		PrimaryKeys: map[string][]string{"subscriptions": {"id"}, "products": {"id"}},
		UniqueKeys: map[string][][]string{
			"subscriptions": {{"id"}, {"rail", "rail_subscription_id"}},
		},
		Columns: map[string][]string{
			"subscriptions": {"id", "merchant_id", "customer_id", "rail", "cancel_feedback", "status"},
			"products":      {"id", "merchant_id"},
		},
		Tables: map[string]struct{}{"subscriptions": {}, "products": {}},
	}
}

func findingRules(q Query, cat *Catalog, t *testing.T) []string {
	t.Helper()
	st, err := q.Parse()
	if err != nil {
		t.Fatalf("parse %s: %v", q.Name, err)
	}
	var rules []string
	for _, f := range structuralFindings(q, st, cat) {
		rules = append(rules, f.Rule)
	}
	return rules
}

func TestStructuralRules(t *testing.T) {
	cat := testCatalog()
	cases := []struct {
		name, kind, sql string
		want            []string
	}{
		{"bounded by indexed key", "many",
			"SELECT id FROM subscriptions WHERE customer_id = $1", nil},
		{"bounded by LIMIT", "many",
			"SELECT id FROM subscriptions WHERE status = 'active' LIMIT 50", nil},
		{"merchant_id alone does not bound", "many",
			"SELECT id FROM subscriptions WHERE merchant_id = $1", []string{RuleUnboundedMany}},
		{"constant equality does not bound", "many",
			"SELECT id FROM subscriptions WHERE status = 'active'", []string{RuleUnboundedMany}},
		{"ANY on a full unique key bounds", "many",
			"SELECT id FROM subscriptions WHERE id = ANY($1)", nil},
		{"ANY on part of a composite key does not bound", "many",
			"SELECT id FROM subscriptions WHERE rail = ANY($1)", []string{RuleUnboundedMany}},
		{"one-row query is exempt from the limit rule", "one",
			"SELECT count(*) FROM subscriptions WHERE status = 'active'", nil},
		{"naked delete", "execrows",
			"DELETE FROM subscriptions WHERE status = 'active'", []string{RuleUnscopedWrite}},
		{"delete pinned to merchant", "execrows",
			"DELETE FROM subscriptions WHERE merchant_id = $1 AND status = 'active'", nil},
		{"delete pinned to a key", "execrows",
			"DELETE FROM subscriptions WHERE id = $1", nil},
		{"claim CTE bounds the write", "many",
			"WITH due AS (SELECT id FROM subscriptions WHERE status = 'active' LIMIT 10 FOR UPDATE SKIP LOCKED) " +
				"UPDATE subscriptions SET status = 'unknown' FROM due WHERE subscriptions.id = due.id RETURNING subscriptions.id", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := findingRules(Query{Name: c.name, Kind: c.kind, SQL: c.sql}, cat, t)
			if len(got) != len(c.want) {
				t.Fatalf("rules = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("rules = %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestMultiStatementRefused(t *testing.T) {
	// The auditor EXPLAINs over the raw simple-query protocol, so a second
	// statement must never get that far.
	_, err := Query{Name: "x", SQL: "SELECT 1; DROP TABLE subscriptions"}.Parse()
	if err == nil {
		t.Fatal("expected multi-statement SQL to be refused")
	}
}

func writeAllowlist(t *testing.T, body string) string {
	t.Helper()
	p := t.TempDir() + "/allow.txt"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAllowlistRejectsUnreviewableEntries(t *testing.T) {
	cases := map[string]string{
		"no section":      "seq-scan Foo # why\n",
		"no rationale":    "## DEBT\nseq-scan Foo\n",
		"empty rationale": "## DEBT\nseq-scan Foo #\n",
		"duplicate":       "## DEBT\nseq-scan Foo # why\nseq-scan Foo # again\n",
		"two subjects":    "## DEBT\nseq-scan Foo Bar # why\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadAllowlist(writeAllowlist(t, body)); err == nil {
				t.Fatal("expected the allowlist to be rejected")
			}
		})
	}
}

func TestAllowlistStaleEntriesAreReported(t *testing.T) {
	a, err := LoadAllowlist(writeAllowlist(t, "## PERMANENT\nseq-scan Used # why\n## DEBT\nseq-scan Unused # why\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.exempt(Finding{Query: "Used", Rule: "seq-scan"}); !ok {
		t.Fatal("Used should be exempt")
	}
	stale := a.Stale()
	if len(stale) != 1 || stale[0].Subject != "Unused" {
		t.Fatalf("stale = %+v, want just Unused", stale)
	}
	if stale[0].Class != "DEBT" {
		t.Fatalf("class = %q, want DEBT", stale[0].Class)
	}
}

func TestMentionsRespectsIdentifierBoundaries(t *testing.T) {
	f := "(price_id = $1)"
	if mentions(f, "id") {
		t.Fatal("`id` must not match inside `price_id`")
	}
	if !mentions(f, "price_id") {
		t.Fatal("`price_id` should match")
	}
}

func TestUnindexedFilterAllowsOnlyGuaranteedPrimaryKeyPoints(t *testing.T) {
	cases := []struct {
		name, filter string
		want         bool
	}{
		{"CAS guards after primary key", "id = $2 AND cancel_feedback = $3", false},
		{"qualified reversed equality", "$2::uuid = s.id AND cancel_feedback = $3", false},
		{"literal primary key", "id = '00000000-0000-0000-0000-000000000001'::uuid AND cancel_feedback = $3", false},
		{"nonunique indexed customer", "customer_id = $2 AND cancel_feedback = $3", true},
		{"primary key only inside OR", "id = $2 OR cancel_feedback = $3", true},
		{"column expression is not unique", "lower(id::text) = $2 AND cancel_feedback = $3", true},
		{"other relation key", "other.id = $2 AND cancel_feedback = $3", true},
		{"set of keys is not one row", "id = ANY($2) AND cancel_feedback = $3", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := Query{Name: tc.name, Kind: "many", SQL: "SELECT * FROM subscriptions s WHERE " + tc.filter}
			structure, err := query.Parse()
			if err != nil {
				t.Fatal(err)
			}
			plan := planNode{NodeType: "Index Scan", RelationName: "subscriptions", Alias: "s", IndexCond: "merchant_id = $1", Filter: tc.filter}
			findings := planFindings(query, structure, plan, testCatalog())
			flagged := false
			for _, finding := range findings {
				if finding.Rule == RuleUnindexedFilter {
					flagged = true
				}
			}
			if flagged != tc.want {
				t.Fatalf("unindexed-filter=%v want=%v: %+v", flagged, tc.want, findings)
			}
		})
	}
}

func TestPrimaryKeyFilterDoesNotExcuseCompositeFragmentsOrSeqScans(t *testing.T) {
	cat := testCatalog()
	scan := planNode{NodeType: "Index Scan", RelationName: "subscriptions", IndexCond: "merchant_id = $1", Filter: "id = $2 AND cancel_feedback = $3"}
	cat.PrimaryKeys["subscriptions"] = []string{"id", "rail"}
	if filterPinsPrimaryKey(scan, cat) {
		t.Fatal("part of a composite primary key must not count as a point lookup")
	}
	cat = testCatalog()
	scan.NodeType = "Seq Scan"
	query := Query{Name: "point-with-seq-scan", Kind: "many", SQL: "SELECT * FROM subscriptions WHERE " + scan.Filter}
	structure, err := query.Parse()
	if err != nil {
		t.Fatal(err)
	}
	findings := planFindings(query, structure, scan, cat)
	for _, finding := range findings {
		if finding.Rule == RuleSeqScan {
			return
		}
	}
	t.Fatalf("primary-key recognition must not suppress physical seq scans: %+v", findings)
}
