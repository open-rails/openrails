package postgresmigrations

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// RLS guard (SEC-16). The rule: every openrails table is merchant-scoped with a
// merchant_isolation policy, or it is an explicitly classified global table.
//
// The table list is DERIVED from every migration's SQL, not hand-maintained —
// the previous hardcoded list only covered 0001, so a merchant-scoped table
// added in a later migration escaped enforcement silently (0005's
// payment_settlement_events shipped with merchant_id and no RLS; nothing
// failed). Exemptions are declared IN the schema as a
// `COMMENT ON TABLE ... 'RLS-exempt by design: ...'` marker, and the exempt set
// is additionally asserted by name below so widening it requires review here.
var rlsExemptTables = []string{
	"merchants",      // the tenant directory itself — the scope, not a scoped row
	"probe_verdicts", // instance-level credential state
	"worker_health",  // per-worker-kind process health
	// #836 instance-level operator kill switch for destructive convergence.
	// Deliberately readable from the no-GUC background connections it polices
	// (intent runner, sweep scheduler); carries no tenant data — the
	// per-merchant half lives in the RLS-protected
	// merchant_destructive_policy.
	"destructive_action_switch",
	// or#837 resume point for capped fan-out sweeps — a worker kind and the
	// merchant id it stopped at. Written from the no-GUC background pass that
	// reads the SECURITY DEFINER work queue; holds no tenant data.
	"worker_sweep_cursors",
}

// minMerchantScopedTables guards against a vacuous pass: if the SQL parsing
// below ever breaks, the derived set collapses and every loop becomes a no-op.
const minMerchantScopedTables = 60

// minParsedIndexes guards the #846 index guard against the same vacuous pass.
const minParsedIndexes = 250

// rlsExemptMarker is the machine-checkable classification a table COMMENT must
// carry to opt out of merchant isolation.
const rlsExemptMarker = "RLS-exempt by design:"

var (
	reCreateTable   = regexp.MustCompile(`(?s)CREATE TABLE (?:IF NOT EXISTS )?openrails\.([a-z0-9_]+) \((.*?)\n\);`)
	reMerchantIDCol = regexp.MustCompile(`(?m)^\s*merchant_id\s+uuid`)
	reAddMerchantID = regexp.MustCompile(`(?s)ALTER TABLE (?:ONLY )?openrails\.([a-z0-9_]+)[^;]*?ADD COLUMN\s+merchant_id\s+uuid`)
	reDropTable     = regexp.MustCompile(`DROP TABLE (?:IF EXISTS )?openrails\.([a-z0-9_]+)`)
	reRenameTable   = regexp.MustCompile(`ALTER TABLE (?:ONLY )?openrails\.([a-z0-9_]+) RENAME TO ([a-z0-9_]+)`)
	reEnableRLS     = regexp.MustCompile(`ALTER TABLE (?:ONLY )?openrails\.([a-z0-9_]+) ENABLE ROW LEVEL SECURITY`)
	reForceRLS      = regexp.MustCompile(`ALTER TABLE (?:ONLY )?openrails\.([a-z0-9_]+) FORCE ROW LEVEL SECURITY`)
	rePolicy        = regexp.MustCompile(`CREATE POLICY merchant_isolation ON openrails\.([a-z0-9_]+)`)
	reDropPolicy    = regexp.MustCompile(`DROP POLICY (?:IF EXISTS )?merchant_isolation ON openrails\.([a-z0-9_]+)`)
	reExemptComment = regexp.MustCompile(`(?s)COMMENT ON TABLE openrails\.([a-z0-9_]+) IS '((?:[^']|'')*)'`)

	// #846 index inventory. Indexes reach the schema three ways: CREATE INDEX,
	// an ALTER TABLE ADD CONSTRAINT PRIMARY KEY/UNIQUE, and an in-body table
	// constraint. All three back an RLS predicate identically, so all three count.
	reCreateIndex = regexp.MustCompile(`(?s)CREATE (?:UNIQUE )?INDEX (?:CONCURRENTLY )?(?:IF NOT EXISTS )?([a-z0-9_]+)\s+ON openrails\.([a-z0-9_]+)\s*(?:USING [a-z0-9_]+\s*)?(\(.*?);`)
	reDropIndex   = regexp.MustCompile(`DROP INDEX (?:CONCURRENTLY )?(?:IF EXISTS )?(?:openrails\.)?([a-z0-9_]+)`)
	reAlterKey    = regexp.MustCompile(`(?s)ALTER TABLE (?:ONLY )?openrails\.([a-z0-9_]+)\s+ADD CONSTRAINT ([a-z0-9_]+) (?:PRIMARY KEY|UNIQUE)\s*(\(.*?);`)
	reInlineKey   = regexp.MustCompile(`(?m)^\s*(?:CONSTRAINT ([a-z0-9_]+) )?(?:PRIMARY KEY|UNIQUE)\s*(\(.*)$`)
	reRenameIndex = regexp.MustCompile(`ALTER INDEX openrails\.([a-z0-9_]+) RENAME TO ([a-z0-9_]+)`)
	reBareIdent   = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

// loadSchema001 reads the consolidated baseline alone (invariants that are
// specifically about the baseline's shape).
func loadSchema001(t *testing.T) string {
	t.Helper()
	b, err := FS.ReadFile("0001_schema.up.sql")
	if err != nil {
		t.Fatalf("read 0001_schema.up.sql: %v", err)
	}
	return string(b)
}

// loadAllSchema concatenates every up migration in apply order — the schema as
// deployed, which is what the RLS guard must reason about.
func loadAllSchema(t *testing.T) string {
	t.Helper()
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // zero-padded prefixes => lexical order == apply order
	if len(names) == 0 {
		t.Fatal("no *.up.sql migrations found")
	}
	var b strings.Builder
	for _, n := range names {
		c, err := FS.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		b.Write(c)
		b.WriteString("\n")
	}
	return b.String()
}

// schemaTables is the derived table inventory of the deployed schema, keyed by
// each table's FINAL name (renames are folded in — RLS state travels with a
// RENAME, so the sets must too).
type schemaTables struct {
	blocks          map[string]string // live openrails tables -> CREATE TABLE body
	merchantScoped  map[string]bool   // carry a merchant_id column
	enable          map[string]bool   // ENABLE ROW LEVEL SECURITY
	force           map[string]bool   // FORCE ROW LEVEL SECURITY
	policy          map[string]bool   // merchant_isolation policy
	rlsExemptMarked map[string]bool   // classified RLS-exempt by a table COMMENT
	indexes         map[string][]schemaIndex
}

// schemaIndex is one parsed index/key: its leading column and whether a WHERE
// predicate makes it partial. A partial index cannot serve the RLS predicate
// for rows outside its predicate.
type schemaIndex struct {
	name    string
	leading string
	partial bool
	unique  bool
	// cols is the parsed column list, normalised. A COALESCE(x, ...) wrapper
	// counts as its first argument, so 0017/0020's total-index technique is
	// recognised as scoping by that column.
	cols []string
}

// scopedBy reports whether the index constrains uniqueness within col.
func (ix schemaIndex) scopedBy(col string) bool {
	for _, c := range ix.cols {
		if c == col {
			return true
		}
	}
	return false
}

// splitTopLevel splits a parenthesised list on commas at depth 0 and reports
// what follows the closing paren (where a WHERE predicate would live).
func splitTopLevel(s string) (cols []string, tail string) {
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
			if depth == 1 {
				start = i + 1
			}
		case ')':
			depth--
			if depth == 0 {
				cols = append(cols, s[start:i])
				return splitCommas(cols[0]), s[i+1:]
			}
		}
	}
	return nil, ""
}

func splitCommas(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// leadingColumn returns the bare column name an index leads with, or "" for an
// expression / functional leading term (which cannot serve `merchant_id = $1`).
func leadingColumn(colList string) string {
	cols, _ := splitTopLevel(colList)
	if len(cols) == 0 {
		return ""
	}
	f := strings.Fields(strings.TrimSpace(cols[0]))
	if len(f) == 0 {
		return ""
	}
	if name := f[0]; reBareIdent.MatchString(name) {
		return name
	}
	return ""
}

func parseIndex(name, colsAndTail string, unique bool) schemaIndex {
	cols, tail := splitTopLevel(colsAndTail)
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		if n := indexColumnName(c); n != "" {
			names = append(names, n)
		}
	}
	return schemaIndex{
		name:    name,
		leading: leadingColumn(colsAndTail),
		partial: strings.Contains(strings.ToUpper(tail), "WHERE"),
		unique:  unique,
		cols:    names,
	}
}

// isUniqueDecl: a PRIMARY KEY constrains uniqueness exactly as a UNIQUE index
// does, so both count for the cross-merchant guard.
func isUniqueDecl(decl string) bool {
	u := strings.ToUpper(decl)
	return strings.Contains(u, "UNIQUE") || strings.Contains(u, "PRIMARY KEY")
}

// indexColumnName extracts the column an index element is keyed on. A bare
// identifier is itself; COALESCE(psp_id, '000…'::uuid) — the total-index
// technique 0017/0020 use to keep a nullable dimension in a UNIQUE index —
// resolves to its first argument.
func indexColumnName(element string) string {
	e := strings.TrimSpace(element)
	if strings.HasPrefix(strings.ToUpper(e), "COALESCE(") {
		inner := e[len("COALESCE("):]
		if i := strings.IndexByte(inner, ','); i > 0 {
			e = strings.TrimSpace(inner[:i])
		}
	}
	f := strings.Fields(e)
	if len(f) == 0 {
		return ""
	}
	if name := f[0]; reBareIdent.MatchString(name) {
		return name
	}
	return ""
}

// hasPlainMerchantIndex: at least one NON-partial index leads with merchant_id,
// so the RLS predicate is index-backed for EVERY row of the table.
func (s schemaTables) hasPlainMerchantIndex(tbl string) bool {
	for _, ix := range s.indexes[tbl] {
		if ix.leading == "merchant_id" && !ix.partial {
			return true
		}
	}
	return false
}

func deriveSchemaTables(t *testing.T, schema string) schemaTables {
	t.Helper()
	s := schemaTables{
		blocks:          map[string]string{},
		merchantScoped:  map[string]bool{},
		enable:          map[string]bool{},
		force:           map[string]bool{},
		policy:          map[string]bool{},
		rlsExemptMarked: map[string]bool{},
		indexes:         map[string][]schemaIndex{},
	}
	for _, m := range reCreateTable.FindAllStringSubmatch(schema, -1) {
		s.blocks[m[1]] = m[2]
		if reMerchantIDCol.MatchString(m[2]) {
			s.merchantScoped[m[1]] = true
		}
		for _, k := range reInlineKey.FindAllStringSubmatch(m[2], -1) {
			s.indexes[m[1]] = append(s.indexes[m[1]], parseIndex(k[1], k[2], isUniqueDecl(k[0])))
		}
	}
	for _, m := range reCreateIndex.FindAllStringSubmatch(schema, -1) {
		s.indexes[m[2]] = append(s.indexes[m[2]], parseIndex(m[1], m[3], isUniqueDecl(m[0][:strings.Index(strings.ToUpper(m[0]), "INDEX")])))
	}
	for _, m := range reAlterKey.FindAllStringSubmatch(schema, -1) {
		s.indexes[m[1]] = append(s.indexes[m[1]], parseIndex(m[2], m[3], isUniqueDecl(m[0])))
	}
	// Renames before drops: 0003's psps hardcut renames indexes that a later
	// migration drops by their NEW name.
	for _, m := range reRenameIndex.FindAllStringSubmatch(schema, -1) {
		for _, ixs := range s.indexes {
			for i := range ixs {
				if ixs[i].name == m[1] {
					ixs[i].name = m[2]
				}
			}
		}
	}
	for _, m := range reDropIndex.FindAllStringSubmatch(schema, -1) {
		for tbl, ixs := range s.indexes {
			kept := ixs[:0]
			for _, ix := range ixs {
				if ix.name != m[1] {
					kept = append(kept, ix)
				}
			}
			s.indexes[tbl] = kept
		}
	}
	for _, m := range reAddMerchantID.FindAllStringSubmatch(schema, -1) {
		s.merchantScoped[m[1]] = true
	}
	for _, m := range reDropTable.FindAllStringSubmatch(schema, -1) {
		delete(s.blocks, m[1])
		delete(s.merchantScoped, m[1])
		delete(s.indexes, m[1])
	}
	for _, m := range reEnableRLS.FindAllStringSubmatch(schema, -1) {
		s.enable[m[1]] = true
	}
	for _, m := range reForceRLS.FindAllStringSubmatch(schema, -1) {
		s.force[m[1]] = true
	}
	for _, m := range rePolicy.FindAllStringSubmatch(schema, -1) {
		s.policy[m[1]] = true
	}
	for _, m := range reDropPolicy.FindAllStringSubmatch(schema, -1) {
		delete(s.policy, m[1])
	}
	for _, m := range reExemptComment.FindAllStringSubmatch(schema, -1) {
		if strings.Contains(m[2], rlsExemptMarker) {
			s.rlsExemptMarked[m[1]] = true
		}
	}
	for _, m := range reRenameTable.FindAllStringSubmatch(schema, -1) {
		s.rename(m[1], m[2])
	}
	return s
}

// rename folds old into new across every set: Postgres carries RLS state,
// policies and comments through an ALTER TABLE ... RENAME TO.
func (s schemaTables) rename(old, next string) {
	if body, ok := s.blocks[old]; ok {
		s.blocks[next] = body
		delete(s.blocks, old)
	}
	if ixs, ok := s.indexes[old]; ok {
		s.indexes[next] = append(s.indexes[next], ixs...)
		delete(s.indexes, old)
	}
	for _, m := range []map[string]bool{s.merchantScoped, s.enable, s.force, s.policy, s.rlsExemptMarked} {
		if m[old] {
			m[next] = true
			delete(m, old)
		}
	}
}

// missingRLS lists the RLS pieces tbl lacks.
func (s schemaTables) missingRLS(tbl string) []string {
	var missing []string
	if !s.enable[tbl] {
		missing = append(missing, "ENABLE ROW LEVEL SECURITY")
	}
	if !s.force[tbl] {
		missing = append(missing, "FORCE ROW LEVEL SECURITY")
	}
	if !s.policy[tbl] {
		missing = append(missing, "CREATE POLICY merchant_isolation")
	}
	return missing
}

// merchantOwnedTables returns the merchant-scoped tables the deployed schema
// declares (sorted). Other schema tests consume it instead of a static list.
func merchantOwnedTables(t *testing.T) []string {
	t.Helper()
	s := deriveSchemaTables(t, loadAllSchema(t))
	out := make([]string, 0, len(s.merchantScoped))
	for tbl := range s.merchantScoped {
		out = append(out, tbl)
	}
	sort.Strings(out)
	return out
}

// #336: there is no default merchant. The consolidated schema creates the
// merchants table but seeds no rows — merchants (and their credit types) are
// provisioned explicitly by the control plane / bootstrap, never defaulted.
func TestConsolidatedSchemaHasNoDefaultMerchant(t *testing.T) {
	schema := loadSchema001(t)

	if !strings.Contains(schema, "CREATE TABLE openrails.merchants") {
		t.Error("001 schema must create openrails.merchants")
	}
	if strings.Contains(schema, "INSERT INTO openrails.merchants") {
		t.Error("001 schema must not seed any merchant row")
	}
}

func TestSchemaMerchantIDColumns(t *testing.T) {
	c := loadAllSchema(t)

	if strings.Contains(c, "WHERE merchant_id IS NULL") {
		t.Error("schema must not contain merchant_id backfill logic")
	}
	if strings.Contains(c, "merchant_id uuid NOT NULL DEFAULT") {
		t.Error("schema must not default merchant_id")
	}
	if strings.Contains(c, "current_setting('app.merchant_id'::text, true), ''::text))::uuid,") {
		t.Error("schema must not default merchant_id from app.merchant_id")
	}
	for tbl, body := range deriveSchemaTables(t, c).blocks {
		if reMerchantIDCol.MatchString(body) && !strings.Contains(body, "merchant_id uuid NOT NULL") {
			t.Errorf("merchant-owned table %q must declare merchant_id uuid NOT NULL", tbl)
		}
	}
}

// TestEveryMerchantIDTableRequiresRLS is the guard: derived from ALL migrations,
// so a merchant-scoped table added in any future migration must ship RLS.
func TestEveryMerchantIDTableRequiresRLS(t *testing.T) {
	c := loadAllSchema(t)
	s := deriveSchemaTables(t, c)

	if len(s.merchantScoped) < minMerchantScopedTables {
		t.Fatalf("derived only %d merchant-scoped tables (< %d): schema parsing is broken, the guard would pass vacuously",
			len(s.merchantScoped), minMerchantScopedTables)
	}
	// Sentinels from migrations beyond the baseline: proves later migrations are
	// actually being read.
	for _, tbl := range []string{"payments", "customer_invoice_profiles", "payment_settlement_events"} {
		if !s.merchantScoped[tbl] {
			t.Fatalf("expected %q in the derived merchant-scoped set", tbl)
		}
	}

	for tbl := range s.merchantScoped {
		if s.rlsExemptMarked[tbl] {
			continue
		}
		if missing := s.missingRLS(tbl); len(missing) > 0 {
			t.Errorf("merchant-scoped table %q missing %v — every merchant_id table needs the standard "+
				"merchant_isolation RLS, or an explicit '%s ...' table COMMENT", tbl, missing, rlsExemptMarker)
		}
	}
}

// TestMerchantIsolationPolicyIsIndexBacked (#846). RLS appends
// `merchant_id = current_setting('app.merchant_id')` to EVERY query on a
// policy-bearing table. If the only merchant_id-leading index is PARTIAL, that
// predicate is unindexed for rows outside the partial predicate and the table
// seq-scans in production — and because SOME index path usually exists, the
// seq-scan detector does not reliably surface it. So assert it structurally:
// every merchant_isolation table needs at least one NON-partial index leading
// with merchant_id.
func TestMerchantIsolationPolicyIsIndexBacked(t *testing.T) {
	s := deriveSchemaTables(t, loadAllSchema(t))

	if len(s.policy) < minMerchantScopedTables {
		t.Fatalf("derived only %d policy-bearing tables (< %d): schema parsing is broken",
			len(s.policy), minMerchantScopedTables)
	}
	// Vacuity guard: index parsing must actually find indexes.
	total := 0
	for _, ixs := range s.indexes {
		total += len(ixs)
	}
	if total < minParsedIndexes {
		t.Fatalf("parsed only %d indexes (< %d): index parsing is broken, the guard would pass vacuously",
			total, minParsedIndexes)
	}

	var missing []string
	for tbl := range s.policy {
		if !s.hasPlainMerchantIndex(tbl) {
			missing = append(missing, tbl)
		}
	}
	sort.Strings(missing)
	for _, tbl := range missing {
		var have []string
		for _, ix := range s.indexes[tbl] {
			if ix.leading == "merchant_id" {
				have = append(have, ix.name+" (PARTIAL)")
			}
		}
		t.Errorf("table %q has a merchant_isolation policy but no NON-partial index leading with merchant_id "+
			"(merchant_id-leading indexes: %v) — its RLS predicate is unindexed and it seq-scans under production RLS",
			tbl, have)
	}
}

// TestRLSExemptionsAreClassifiedAndReviewed: every table without a
// merchant_isolation policy must declare itself RLS-exempt in a table COMMENT,
// and the exempt set must be exactly the reviewed list — a new global table
// fails here until it is added deliberately.
func TestRLSExemptionsAreClassifiedAndReviewed(t *testing.T) {
	s := deriveSchemaTables(t, loadAllSchema(t))

	var unpoliced []string
	for tbl := range s.blocks {
		if !s.policy[tbl] {
			unpoliced = append(unpoliced, tbl)
		}
	}
	sort.Strings(unpoliced)

	want := append([]string{}, rlsExemptTables...)
	sort.Strings(want)
	if strings.Join(unpoliced, ",") != strings.Join(want, ",") {
		t.Errorf("tables without a merchant_isolation policy = %v, reviewed exemption list = %v; "+
			"a merchant-scoped table needs RLS, a global one needs adding here plus an '%s ...' table COMMENT",
			unpoliced, want, rlsExemptMarker)
	}
	for _, tbl := range rlsExemptTables {
		if _, ok := s.blocks[tbl]; !ok {
			t.Errorf("exempt table %q does not exist in the schema", tbl)
		}
		if !s.rlsExemptMarked[tbl] {
			t.Errorf("exempt table %q lacks a COMMENT ON TABLE ... '%s ...' marker", tbl, rlsExemptMarker)
		}
		if s.merchantScoped[tbl] {
			t.Errorf("exempt table %q carries merchant_id: it is tenant data and must be RLS-protected", tbl)
		}
	}
}

func TestConsolidatedSchemaClassifiesGlobalTables(t *testing.T) {
	c := loadAllSchema(t)
	for _, want := range []string{
		"COMMENT ON TABLE openrails.merchants IS 'Merchant / billing-namespace directory",
		"GLOBAL (control-plane) table",
		"COMMENT ON TABLE openrails.probe_verdicts IS 'Cached NMI test-mode probe verdicts",
		"RLS-exempt by design: instance-level credential state, not tenant data",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("schema missing global/control-plane table classification %q", want)
		}
	}
}

func TestConsolidatedSchemaHasNoMerchantSettingsTable(t *testing.T) {
	c := loadSchema001(t)
	if strings.Contains(c, "CREATE TABLE openrails.merchant_settings") {
		t.Error("merchant_settings is not a live table; merchant_configurations is the RLS-protected merchant settings table")
	}
	if !strings.Contains(c, "CREATE TABLE openrails.merchant_configurations") {
		t.Error("001 schema missing merchant_configurations")
	}
	if !strings.Contains(c, "CREATE POLICY merchant_isolation ON openrails.merchant_configurations") {
		t.Error("merchant_configurations must remain RLS protected")
	}
}

func TestConsolidatedSchemaUsesCustomerUniques(t *testing.T) {
	c := loadSchema001(t)

	for _, forbidden := range []string{
		"uq_payment_methods_tenant_user_vault",
		"uq_subscriptions_tenant_user_product_lifecycle",
		"uq_entitlements_tenant_active",
		"uq_rail_customers_tenant_user_rail",
		" user_id text",
	} {
		if strings.Contains(c, forbidden) {
			t.Errorf("001 schema must not keep legacy user-scoped artifact %q", forbidden)
		}
	}
	for _, want := range []string{
		"uq_payment_methods_customer_instrument_legacy",
		"uq_subscriptions_customer_product_lifecycle",
		"uq_entitlements_customer_active",
		"uq_payments_merchant_rail_transaction",
		"uq_rail_customer_accounts_customer_rail",
		"entitlements_customer_no_overlap",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing final customer invariant %q", want)
		}
	}
}

func TestConsolidatedSchemaUsesMerchantPermissionGroup(t *testing.T) {
	c := loadSchema001(t)

	// The consolidated baseline reflects the final schema: permission_group_id.
	for _, want := range []string{
		"permission_group_id text",
		"idx_merchants_permission_group_id",
		"COMMENT ON COLUMN openrails.merchants.permission_group_id",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing merchant permission group invariant %q", want)
		}
	}
	legacyOwnerColumn := "owner_" + "o" + "rg_id"
	for _, forbidden := range []string{
		"owner_tenant_id",
		"idx_merchants_owner_tenant_id",
		legacyOwnerColumn + " text",
		"UNIQUE INDEX idx_merchants_" + legacyOwnerColumn,
		"UNIQUE (" + legacyOwnerColumn + ")",
	} {
		if strings.Contains(c, forbidden) {
			t.Errorf("001 schema must not keep legacy owner artifact %q", forbidden)
		}
	}
}

// TestUniqueIndexesAreMerchantScoped is the GAP-10 / SEC-24 guard. A UNIQUE
// index on a merchant-owned table must constrain uniqueness WITHIN a merchant.
// A primary key on a surrogate uuid is fine (uuidv7 collisions are not a
// tenancy question); anything else must carry merchant_id.
//
// checkout_sessions was the last violation: (rail, reference) and
// (rail, transaction_id) let one merchant's session block another merchant's
// insert and squat its provider reference. 0020 scopes both.
func TestUniqueIndexesAreMerchantScoped(t *testing.T) {
	s := deriveSchemaTables(t, loadAllSchema(t))

	uniques := 0
	for _, ixs := range s.indexes {
		for _, ix := range ixs {
			if ix.unique {
				uniques++
			}
		}
	}
	// Vacuity guard: if uniqueness parsing breaks, every check below is a no-op.
	if uniques < 80 {
		t.Fatalf("parsed only %d unique indexes (< 80): uniqueness parsing is broken, the guard would pass vacuously", uniques)
	}

	var violations []string
	for tbl, ixs := range s.indexes {
		if !s.merchantScoped[tbl] {
			continue
		}
		for _, ix := range ixs {
			if !ix.unique || ix.scopedBy("merchant_id") {
				continue
			}
			// ONE exemption list, shared with the live-database guard in
			// internal/invariantaudit (unique_scope_exemptions.go).
			if UniqueScopeExempt(ix.name, ix.cols) {
				continue
			}
			violations = append(violations, tbl+"."+ix.name+" "+strings.Join(ix.cols, ","))
		}
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("UNIQUE index %s is not scoped by merchant_id — under RLS the conflicting row is invisible, "+
			"so one merchant can block another's insert and squat its provider reference (GAP-10 / SEC-24). "+
			"Lead the index with merchant_id (COALESCE a nullable PSP dimension to the nil uuid, as 0017/0020 do), "+
			"or add it to CrossMerchantUniqueExemptions with a reason.", v)
	}
}

// TestCurrencyColumnsCarryShapeCheck is the GAP-6 / or#832 guard. Every
// currency column must carry the 0020 shape CHECK, so a new table cannot
// re-open the drift that made a dev DB's payments.currency 100% lowercase.
//
// The CHECK constrains SHAPE (ISO-4217 alpha-3 upper, or a qualified
// custom-credit unit slug/name); MEMBERSHIP stays in the Go registry, which is
// where per-currency scale lives and where it can change without a migration.
func TestCurrencyColumnsCarryShapeCheck(t *testing.T) {
	schema := loadAllSchema(t)
	s := deriveSchemaTables(t, schema)

	reCurrencyCol := regexp.MustCompile(`(?m)^\s*currency\s+text`)

	var withCurrency []string
	for tbl, body := range s.blocks {
		if reCurrencyCol.MatchString(body) {
			withCurrency = append(withCurrency, tbl)
		}
	}
	sort.Strings(withCurrency)
	// Vacuity guard: CUR-1 counts 16+ currency columns.
	if len(withCurrency) < 16 {
		t.Fatalf("found only %d currency columns (< 16): column parsing is broken, the guard would pass vacuously",
			len(withCurrency))
	}

	// 0020 adds the constraint in a DO loop over information_schema, so the
	// coverage assertion is that the loop exists and is unconditional over
	// every currency column — plus that nothing later drops one.
	if !strings.Contains(schema, "_currency_shape") {
		t.Fatal("no currency shape CHECK found in any migration (GAP-6): 0020 must add one per currency column")
	}
	reDropShape := regexp.MustCompile(`DROP CONSTRAINT (?:IF EXISTS )?([a-z0-9_]+_currency_shape)`)
	for _, m := range reDropShape.FindAllStringSubmatch(schema, -1) {
		t.Errorf("migration drops the currency shape CHECK %q — GAP-6 requires every currency column keep it", m[1])
	}
	// The loop must be driven by information_schema, not a hand-written list:
	// a hand-written list is exactly how 0005's payment_settlement_events
	// escaped the RLS guard (GAP-4).
	if !strings.Contains(schema, "column_name = 'currency'") {
		t.Error("the currency shape CHECK must be applied by iterating information_schema for column_name='currency', " +
			"so a currency column added by a later migration is covered automatically (the lesson of GAP-4)")
	}
}
