package clickhousemigrations

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
)

func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func loadClickHouseSchemaMigration(t *testing.T) string {
	t.Helper()

	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var sqlFiles []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			sqlFiles = append(sqlFiles, entry.Name())
		}
	}
	if len(sqlFiles) != 1 || sqlFiles[0] != "001_schema.up.sql" {
		t.Fatalf("embedded ClickHouse SQL files = %v, want [001_schema.up.sql]", sqlFiles)
	}

	migrations, err := migratekit.LoadFromFS(FS)
	if err != nil {
		t.Fatalf("LoadFromFS: %v", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("loaded ClickHouse migrations = %d, want 1", len(migrations))
	}
	if migrations[0].Name != "001_schema.up.sql" {
		t.Fatalf("loaded ClickHouse migration = %q, want 001_schema.up.sql", migrations[0].Name)
	}
	return migrations[0].Content
}

func TestClickHouseSchemaConsolidationInvariants(t *testing.T) {
	schema := normalizeSQL(loadClickHouseSchemaMigration(t))

	for _, table := range []string{
		"subscription_events",
		"payment_events",
		"webhook_events",
		"acu_events",
		"chargeback_events",
		"premium_status_daily",
		"daily_metrics",
	} {
		want := "create table if not exists " + table + " {{on_cluster}}"
		if !strings.Contains(schema, want) {
			t.Errorf("consolidated ClickHouse schema missing table %q", table)
		}
	}

	for _, want := range []string{
		"merchant_id string default '00000000-0000-0000-0000-000000000001'",
		"create materialized view if not exists mv_daily_metrics {{on_cluster}} to daily_metrics",
		"order by (snapshot_date, currency, merchant_id)",
		"window w as (partition by currency, merchant_id order by snapshot_date rows between unbounded preceding and current row)",
		"index idx_subscription_events_merchant (merchant_id) type set(0) granularity 1",
		"index idx_payment_events_merchant (merchant_id) type set(0) granularity 1",
		"index idx_webhook_events_merchant (merchant_id) type set(0) granularity 1",
		"index idx_acu_events_merchant (merchant_id) type set(0) granularity 1",
		"index idx_chargeback_events_merchant (merchant_id) type set(0) granularity 1",
		"index idx_premium_status_daily_merchant (merchant_id) type set(0) granularity 1",
		"alter table subscription_events rename column if exists tenant_id to merchant_id",
		"drop table if exists mv_daily_metrics",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("consolidated ClickHouse schema missing invariant %q", want)
		}
	}

	for _, forbidden := range []string{
		"migration 002",
		"migration 004",
		"migration 005",
		"issue #232",
		"order by (snapshot_date, currency) partition by",
	} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("consolidated ClickHouse schema contains historical marker %q", forbidden)
		}
	}
}
