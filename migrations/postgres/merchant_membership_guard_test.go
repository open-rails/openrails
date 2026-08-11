package postgresmigrations

// or#914 item 4: the no-parallel-membership guard. Merchant identity IS the
// merchant's authkit permission group — members, roles, and invites live in
// authkit's profiles.* schema, addressed through control-plane calls, never
// mirrored into openrails tables. This guard fails the moment any openrails
// migration grows membership-shaped state (a user/role/invite/team column or
// table), so a parallel team system cannot creep back in one column at a
// time. Like the RLS guard above it reasons over the DEPLOYED schema derived
// from every migration, so it needs no database and runs in the unit shards.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reMembershipName is the membership shape: any identifier that smells like
// user/role/invite/team state.
var reMembershipName = regexp.MustCompile(`(?:^|_)(user|users|member|members|membership|invite|invites|invitee|role|roles|team|teams|staff|grantee|owner)(?:_|$)`)

// membershipAllowlist names the reviewed non-membership uses of those words.
// Adding an entry here is a DECISION that the column is not merchant-team
// state — say why.
var membershipAllowlist = map[string]string{
	// The paying CUSTOMER's receipt email on a subscription row — end-user
	// billing data, not merchant staff state.
	"subscriptions.user_email": "customer receipt email, not team state",
}

var (
	reAddAnyColumn = regexp.MustCompile(`(?s)ALTER TABLE (?:ONLY )?openrails\.([a-z0-9_]+)[^;]*?ADD COLUMN\s+(?:IF NOT EXISTS\s+)?([a-z0-9_]+)`)
	reDropAnyCol   = regexp.MustCompile(`(?s)ALTER TABLE (?:ONLY )?openrails\.([a-z0-9_]+)[^;]*?DROP COLUMN\s+(?:IF EXISTS\s+)?([a-z0-9_]+)`)
)

// membershipViolations derives every openrails table's live columns from the
// concatenated migration SQL (CREATE TABLE bodies + ADD COLUMN, minus DROP
// COLUMN/TABLE, table RENAMEs folded by deriveSchemaTables) and reports
// membership-shaped table or column names outside the allowlist.
func membershipViolations(t *testing.T, schema string) []string {
	t.Helper()
	s := deriveSchemaTables(t, schema)

	cols := map[string]map[string]bool{} // table -> live column set
	for tbl, body := range s.blocks {
		cols[tbl] = map[string]bool{}
		for _, c := range tableColumns(body) {
			cols[tbl][c] = true
		}
	}
	for _, m := range reAddAnyColumn.FindAllStringSubmatch(schema, -1) {
		if _, live := s.blocks[m[1]]; live {
			cols[m[1]][m[2]] = true
		}
	}
	for _, m := range reDropAnyCol.FindAllStringSubmatch(schema, -1) {
		if set, ok := cols[m[1]]; ok {
			delete(set, m[2])
		}
	}

	var out []string
	for tbl, set := range cols {
		if reMembershipName.MatchString(tbl) {
			out = append(out, fmt.Sprintf("table %s: membership-shaped table name", tbl))
		}
		for col := range set {
			if !reMembershipName.MatchString(col) {
				continue
			}
			key := tbl + "." + col
			if _, ok := membershipAllowlist[key]; ok {
				continue
			}
			out = append(out, fmt.Sprintf("column %s: membership-shaped column outside the or#914 allowlist", key))
		}
	}
	sort.Strings(out)
	return out
}

// tableColumns extracts the column names of a derived CREATE TABLE body (the
// same normalized body deriveSchemaTables stores: one declaration per line,
// ADD COLUMNs appended, DROP COLUMNs removed).
func tableColumns(body string) []string {
	var cols []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if line == "" {
			continue
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "CONSTRAINT "),
			strings.HasPrefix(up, "PRIMARY KEY"),
			strings.HasPrefix(up, "UNIQUE"),
			strings.HasPrefix(up, "FOREIGN KEY"),
			strings.HasPrefix(up, "CHECK"),
			strings.HasPrefix(up, "LIKE "),
			strings.HasPrefix(up, "EXCLUDE"):
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 || !reBareIdent.MatchString(f[0]) {
			continue
		}
		cols = append(cols, f[0])
	}
	return cols
}

// TestNoParallelMerchantMembershipState is the guard: the deployed openrails
// schema carries no membership-shaped state.
func TestNoParallelMerchantMembershipState(t *testing.T) {
	if v := membershipViolations(t, loadAllSchema(t)); len(v) != 0 {
		t.Fatalf("or#914: openrails tables must not carry merchant user/role/invite state — "+
			"the merchant's authkit permission group is the ONE team system. Either the state belongs "+
			"in authkit (it almost certainly does) or a reviewed non-membership use gets an allowlist "+
			"entry in merchant_membership_guard_test.go naming why.\n\n%s", strings.Join(v, "\n"))
	}
}

// TestMembershipGuardDiscriminates proves the guard goes RED against
// hand-built violations of each shape it claims to catch — a guard that
// cannot fail is decoration (test doctrine: provably discriminating).
func TestMembershipGuardDiscriminates(t *testing.T) {
	base := loadAllSchema(t)
	for name, snippet := range map[string]string{
		"membership table": `CREATE TABLE openrails.merchant_members (
    merchant_id uuid NOT NULL,
    user_id uuid NOT NULL
);`,
		"invite table": `CREATE TABLE openrails.pending_invites (
    id uuid NOT NULL
);`,
		"role column on merchants": "ALTER TABLE openrails.merchants\n    ADD COLUMN default_role text;",
		"user column via CREATE": `CREATE TABLE openrails.merchant_settings_x (
    merchant_id uuid NOT NULL,
    invited_user_id uuid
);`,
		"owner column via ADD COLUMN": "ALTER TABLE openrails.products\n    ADD COLUMN owner_user_id uuid;",
	} {
		if v := membershipViolations(t, base+"\n"+snippet+"\n"); len(v) == 0 {
			t.Errorf("guard failed to flag a hand-built violation (%s):\n%s", name, snippet)
		}
	}
	// And the allowlisted column is genuinely load-bearing: without its entry
	// the guard must go red on today's schema.
	saved := membershipAllowlist["subscriptions.user_email"]
	delete(membershipAllowlist, "subscriptions.user_email")
	defer func() { membershipAllowlist["subscriptions.user_email"] = saved }()
	if v := membershipViolations(t, base); len(v) == 0 {
		t.Error("allowlist entry subscriptions.user_email is stale — nothing matches it; delete it")
	}
}
