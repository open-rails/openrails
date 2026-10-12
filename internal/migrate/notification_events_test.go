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
	"github.com/open-rails/openrails/internal/notificationevents"
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
	global, err := notificationevents.New(database, nil, "host:global", "portal")
	require.NoError(t, err)
	require.NoError(t, global.Register(ctx))
	events, err := global.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, events, 2)
	for _, e := range events {
		require.NotContains(t, string(e.Data), "user_email")
		require.Equal(t, billing.MerchantID(mid), e.MerchantID)
	}
	var count int
	require.NoError(t, pool.QueryRow(ctx, sql(`SELECT count(*) FROM billing.notifications WHERE recipient_kind='merchant'`)).Scan(&count))
	require.Equal(t, 1, count, "operator alerts survive migration")
	_, err = pool.Exec(ctx, sql(`UPDATE billing.notification_events SET event_type='changed' WHERE merchant_id=$1`), mid)
	require.Error(t, err, "facts are immutable")
	require.NoError(t, global.Acknowledge(ctx, []billing.NotificationEventRef{{MerchantID: billing.MerchantID(mid), ID: billing.NotificationID(first)}}))
	now := time.Now().UTC()
	n, err := notificationevents.Prune(ctx, database, now, 1000)
	require.NoError(t, err)
	require.Zero(t, n, "acknowledgment starts grace, not immediate deletion")
	n, err = notificationevents.Prune(ctx, database, now.Add(31*24*time.Hour), 1000)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	events, err = global.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, billing.NotificationID(second), events[0].ID)
	// GC and registration use the same transaction fence. A registered but not\n // yet committed consumer must prevent old eligibility from being reclaimed.
	require.NoError(t, global.Acknowledge(ctx, []billing.NotificationEventRef{{MerchantID: billing.MerchantID(mid), ID: billing.NotificationID(second)}}))
	_, err = notificationevents.Prune(ctx, database, now, 1000)
	require.NoError(t, err)
	tx, err := database.DataPool().Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	q := gen.New(tx)
	require.NoError(t, q.LockNotificationConsumers(ctx))
	require.NoError(t, q.RegisterNotificationConsumer(ctx, gen.RegisterNotificationConsumerParams{Owner: "host:global", Name: "new-consumer"}))
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := notificationevents.Prune(ctx, database, now.Add(31*24*time.Hour), 1000)
		done <- err
	}()
	<-started
	// This timeout is a bounded proof of blocking while the transaction is held.
	select {
	case err := <-done:
		t.Fatalf("GC passed uncommitted registration: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, tx.Commit(ctx))
	require.NoError(t, <-done)
	newcomer, err := notificationevents.New(database, nil, "host:global", "new-consumer")
	require.NoError(t, err)
	events, err = newcomer.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NoError(t, newcomer.Retire(ctx))
	require.NoError(t, newcomer.Register(ctx))
	events, err = newcomer.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, events, 1, "new incarnation replays retained events")
	// Physical customer erasure cascades through facts and all consumer ACKs.
	exec(`DELETE FROM billing.customers WHERE merchant_id=$1 AND id=$2`, mid, customer)
	events, err = global.Pending(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, events)
	require.NoError(t, pool.QueryRow(ctx, sql(`SELECT count(*) FROM billing.notification_event_acks`)).Scan(&count))
	require.Zero(t, count)
}
