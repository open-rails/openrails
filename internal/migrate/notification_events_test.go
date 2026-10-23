package migrate

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventlog"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/stretchr/testify/require"
)

func TestNotificationEventsMigrationAndRetention(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "notification_events_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	migrations, err := loadMigrations(schema)
	require.NoError(t, err)
	before := -1
	for i, m := range migrations {
		if strings.HasPrefix(m.Name, "0073_") {
			before = i
		}
	}
	require.Positive(t, before)
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	require.NoError(t, err)
	defer migrator.Close()
	migrator.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	require.NoError(t, migrator.ApplyMigrations(ctx, migrations[:before]))
	sql := func(s string) string { return strings.ReplaceAll(s, "billing.", schema+".") }
	exec := func(s string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql(s), args...)
		require.NoError(t, err)
	}
	mid, customer, first, second := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'notifications','active')`, mid)
	exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
	exec(`INSERT INTO billing.notifications(merchant_id,id,customer_id,event_type,data,read_at) VALUES($1,$2,$3,'order_due','{"amount_due":"9007199254740993","user_email":"old@example.test"}',now()),($1,$4,$3,'future_kind','{"future":true}',NULL)`, mid, first, customer, second)
	exec(`INSERT INTO billing.notifications(merchant_id,id,recipient_kind,event_type,title,body,severity,data) VALUES($1,$2,'merchant','operator.alert','Review','Private evidence','high','{}')`, mid, uuid.New())
	require.NoError(t, migrator.ApplyMigrations(ctx, migrations))
	database, err := db.NewWithPGXPool(pool, schema)
	require.NoError(t, err)
	read := func() billing.EventPage {
		page, err := eventlog.List(ctx, database, billing.MerchantID(mid), billing.EventQuery{}, retention.EventLog, time.Now())
		require.NoError(t, err)
		return page
	}
	page := read()
	require.Len(t, page.Events, 2)
	for _, e := range page.Events {
		require.NotContains(t, string(e.Data), "user_email")
		require.Equal(t, billing.MerchantID(mid), e.MerchantID)
	}
	var count int
	require.NoError(t, pool.QueryRow(ctx, sql(`SELECT count(*) FROM billing.notifications WHERE recipient_kind='merchant'`)).Scan(&count))
	require.Equal(t, 1, count, "operator alerts survive migration")
	_, err = pool.Exec(ctx, sql(`UPDATE billing.notification_events SET event_type='changed' WHERE merchant_id=$1`), mid)
	require.Error(t, err, "producer facts are immutable")
	_, err = pool.Exec(ctx, sql(`UPDATE billing.event_log SET event_type='changed' WHERE merchant_id=$1`), mid)
	require.Error(t, err, "public facts are immutable")
	q := database.GenDirectory()
	// An imported historical occurrence is fresh dedupe evidence now, not due
	// for immediate retirement merely because its business date is old.
	exec(`INSERT INTO billing.host_outbox(merchant_id,event_type,subject_type,subject_id,occurred_at,data,dedupe_key)
        VALUES($1,'customer.network_unlinked','customer',$2,now()-interval '10 years','{"revoked_by":"customer"}','backdated-import')`, mid, customer)
	dropped, err := q.DeleteHostLifecycleEventsBefore(ctx, gen.DeleteHostLifecycleEventsBeforeParams{MerchantID: mid, Cutoff: time.Now().Add(-retention.EventLog), RowLimit: 1000})
	require.NoError(t, err)
	require.Zero(t, dropped, "dedupe retention starts at ingestion, not historical occurrence")
	dropped, err = q.DeleteHostLifecycleEventsBefore(ctx, gen.DeleteHostLifecycleEventsBeforeParams{MerchantID: mid, Cutoff: time.Now().Add(time.Hour), RowLimit: 1000})
	require.NoError(t, err)
	require.EqualValues(t, 1, dropped)
	expected := read()
	require.Len(t, expected.Events, 3, "producer cleanup preserves its retained public snapshot")
	n, err := q.PruneNotificationJournal(ctx, gen.PruneNotificationJournalParams{Cutoff: time.Now().Add(-retention.EventLog), RowLimit: 1000})
	require.NoError(t, err)
	require.Zero(t, n, "fresh ingestion has its full dedupe retention")
	n, err = q.PruneNotificationJournal(ctx, gen.PruneNotificationJournalParams{Cutoff: time.Now().Add(time.Hour), RowLimit: 1000})
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	require.Equal(t, expected, read(), "producer cleanup cannot consume a reader's public facts")
	exec(`DELETE FROM billing.customers WHERE merchant_id=$1 AND id=$2`, mid, customer)
	require.Equal(t, expected, read(), "committed identity snapshot survives deletion; the receiving app enforces erasure")
	for _, name := range []string{"notification_consumers", "notification_event_acks"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+"."+name).Scan(&exists))
		require.False(t, exists, name+" is retired")
	}
}
