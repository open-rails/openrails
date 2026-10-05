package retention

import (
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

func baseline(t *testing.T) string {
	t.Helper()
	var all strings.Builder
	err := fs.WalkDir(postgresmigrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(postgresmigrations.FS, path)
		all.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return all.String()
}

var (
	createTable  = regexp.MustCompile(`(?m)^CREATE TABLE billing\.(\w+) \(`)
	partitionBy  = regexp.MustCompile(`(?m)^\) PARTITION BY RANGE \((\w+)\);$`)
	tableComment = regexp.MustCompile(`(?m)^COMMENT ON TABLE billing\.(\w+) IS '((?:[^']|'')*)';$`)
	guardTrigger = regexp.MustCompile(`(?s)BEFORE DELETE ON billing\.(\w+)\s+FOR EACH ROW(?: WHEN \([^)]*\))? EXECUTE FUNCTION billing\.guard_retention_delete\('(\w+)', '(\d+) days'\);`)
	outboxIndex  = regexp.MustCompile(`(?s)CREATE INDEX provider_intents_updated_at_idx .*?intent_type IN \(([^)]*)\);`)
	holdLifetime = regexp.MustCompile(`admission_operations_hold_lifetime_check CHECK \(expires_at IS NULL OR expires_at <= admitted_at \+ interval '(\d+) hours'\)`)
)

// A table added to the baseline without a retention class fails here.
func TestEveryTableHasARetentionClass(t *testing.T) {
	var tables []string
	for _, m := range createTable.FindAllStringSubmatch(baseline(t), -1) {
		tables = append(tables, m[1])
	}
	if len(tables) == 0 {
		t.Fatal("parsed no tables from the migrations")
	}
	classified := make([]string, 0, len(Tables))
	for name, table := range Tables {
		classified = append(classified, name)
		switch table.Class {
		case Permanent, PartitionedMonthly, Rows:
			if table.Rule == "" {
				t.Errorf("%s: class %s needs the rule its table comment states", name, table.Class)
			}
		case State:
			if table.Rule != "" {
				t.Errorf("%s: a state table has no retention rule", name)
			}
		default:
			t.Errorf("%s: unknown retention class %q", name, table.Class)
		}
	}
	sort.Strings(tables)
	sort.Strings(classified)
	if !slices.Equal(tables, classified) {
		t.Errorf("retention.Tables must classify exactly the migrations' tables\nmigrations: %v\nclassified: %v", tables, classified)
	}
}

// The schema says what the code does: a pruned or permanent table's comment
// carries its rule, and no other table claims one.
func TestTableCommentsStateRetention(t *testing.T) {
	comments := map[string]string{}
	for _, m := range tableComment.FindAllStringSubmatch(baseline(t), -1) {
		comments[m[1]] = strings.ReplaceAll(m[2], "''", "'")
	}
	for name, table := range Tables {
		comment := comments[name]
		if table.Class == State {
			if strings.Contains(comment, "Retention:") {
				t.Errorf("%s is a state table but its comment states a retention", name)
			}
			continue
		}
		if want := "Retention: " + table.Rule; !strings.HasSuffix(comment, want) {
			t.Errorf("COMMENT ON TABLE %s must end with %q", name, want)
		}
	}
}

func TestPartitionedTablesMatchTheBaseline(t *testing.T) {
	// Each PARTITION BY closes the CREATE TABLE that opened last before it.
	sql := baseline(t)
	declared := map[string]string{}
	for _, m := range partitionBy.FindAllStringSubmatchIndex(sql, -1) {
		opened := createTable.FindAllStringSubmatch(sql[:m[0]], -1)
		declared[opened[len(opened)-1][1]] = sql[m[2]:m[3]]
	}
	listed := map[string]string{}
	for _, p := range Partitioned {
		listed[p.Table] = p.Key
		if Tables[p.Table].Class != PartitionedMonthly {
			t.Errorf("%s is partitioned but classified %s", p.Table, Tables[p.Table].Class)
		}
	}
	for name, table := range Tables {
		if _, ok := listed[name]; table.Class == PartitionedMonthly && !ok {
			t.Errorf("%s is classified partitioned but missing from Partitioned", name)
		}
	}
	if len(declared) != len(listed) {
		t.Fatalf("baseline partitions %v, Partitioned lists %v", declared, listed)
	}
	for name, key := range listed {
		if declared[name] != key {
			t.Errorf("%s: baseline partitions by %q, Partitioned says %q", name, declared[name], key)
		}
	}
}

// The delete guards and the hold-lifetime check carry periods in the baseline;
// they are the constants here, in whole days.
func TestBaselinePeriodsAreTheConstants(t *testing.T) {
	sql := baseline(t)
	want := map[string]time.Duration{
		"subscription_status_transitions": SubscriptionTransitions,
		"maintenance_runs":                ReconciliationRuns,
		"cost_observations":               CostObservations,
	}
	got := map[string]time.Duration{}
	for _, m := range guardTrigger.FindAllStringSubmatch(sql, -1) {
		days, err := strconv.Atoi(m[3])
		if err != nil {
			t.Fatal(err)
		}
		got[m[1]] = time.Duration(days) * Day
		if Tables[m[1]].Class != Rows {
			t.Errorf("%s has a retention delete guard but is classified %s", m[1], Tables[m[1]].Class)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("baseline guards %v, expected %v", got, want)
	}
	for table, period := range want {
		if got[table] != period {
			t.Errorf("%s: the baseline guard allows deletes after %s, the constant is %s", table, got[table], period)
		}
	}
	m := holdLifetime.FindStringSubmatch(sql)
	if m == nil {
		t.Fatal("admission_operations_hold_lifetime_check not found in the baseline")
	}
	if hours, _ := strconv.Atoi(m[1]); time.Duration(hours)*time.Hour != AdmissionMaxHold {
		t.Errorf("baseline hold lifetime is %s hours, AdmissionMaxHold is %s", m[1], AdmissionMaxHold)
	}
	if Admissions < AdmissionMaxWindow+AdmissionMaxHold {
		t.Errorf("an admission must outlive its window and its hold: kept %s", Admissions)
	}
}

func TestPartitionCalendar(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	usage, admissions := Partitioned[0], Partitioned[1]

	// Usage of January 2024 is invoiced by the end of February and kept 24
	// months past that: its partition goes on 1 March 2026, not a day before.
	january := at("2024-02-01T00:00:00Z") // the partition's upper bound
	if !UsageDropBefore(at("2026-02-28T23:59:59Z")).Before(january) {
		t.Error("the January 2024 usage partition was dropped before March 2026")
	}
	if UsageDropBefore(at("2026-03-01T00:00:00Z")).Before(january) {
		t.Error("the January 2024 usage partition survived 1 March 2026")
	}
	// Month ends do not overflow into the next month.
	if got := UsageDropBefore(at("2028-03-31T12:00:00Z")); !got.Equal(at("2026-02-01T00:00:00Z")) {
		t.Errorf("UsageDropBefore(31 March 2028) = %s", got)
	}

	// Writable months: back to the ingest window, ahead two months.
	from, through := usage.Range(at("2026-10-04T10:00:00Z"))
	if !from.Equal(at("2026-08-01T00:00:00Z")) || !through.Equal(at("2026-12-01T00:00:00Z")) {
		t.Errorf("usage range = [%s, %s]", from, through)
	}
	from, through = admissions.Range(at("2026-10-04T10:00:00Z"))
	if !from.Equal(at("2026-10-01T00:00:00Z")) || !through.Equal(at("2026-12-01T00:00:00Z")) {
		t.Errorf("admissions range = [%s, %s]", from, through)
	}
	// A restore may need every retained month.
	from, _ = usage.RetainedRange(at("2026-10-04T10:00:00Z"))
	if !from.Equal(at("2024-09-01T00:00:00Z")) {
		t.Errorf("usage retained range starts %s", from)
	}
	if got := AdmissionsDropBefore(at("2026-10-04T10:00:00Z")); !got.Equal(at("2026-08-04T10:00:00Z")) {
		t.Errorf("AdmissionsDropBefore = %s", got)
	}
}

// The intents retention may delete are listed once in the baseline's partial
// index and once here. A type added to either alone fails: an intent that is a
// record of money must never drift into the deletable set.
func TestOutboxIntentTypesMatchTheBaseline(t *testing.T) {
	m := outboxIndex.FindStringSubmatch(baseline(t))
	if m == nil {
		t.Fatal("provider_intents_updated_at_idx not found in the baseline")
	}
	var indexed []string
	for _, q := range regexp.MustCompile(`'(\w+)'`).FindAllStringSubmatch(m[1], -1) {
		indexed = append(indexed, q[1])
	}
	listed := slices.Clone(OutboxIntentTypes)
	sort.Strings(indexed)
	sort.Strings(listed)
	if !slices.Equal(indexed, listed) {
		t.Errorf("the baseline index and OutboxIntentTypes differ\nbaseline: %v\nlisted:   %v", indexed, listed)
	}
	// The record-of-money types are never deletable.
	for _, permanent := range []string{
		"subscription_collection", "initial_membership", "manual_rebill", "invoice_collection",
		"nmi_sale", "custodian_sale", "nmi_upgrade", "stripe_tier_change", "solana_pull",
		"nmi_refund", "stripe_refund", "ccbill_refund", "nmi_vault_delete", "hyperswitch_method_delete",
	} {
		if slices.Contains(listed, permanent) {
			t.Errorf("%s is a record of money, a membership or an erasure and must not be an outbox type", permanent)
		}
	}
}
