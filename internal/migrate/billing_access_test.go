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
	"github.com/stretchr/testify/require"
)

func TestBillingAccessMigrationPreservesPaidTerms(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required for the populated migration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "duration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	migrations, err := loadMigrations(schema)
	require.NoError(t, err)
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	require.NoError(t, err)
	defer migrator.Close()
	migrator.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	require.NoError(t, migrator.ApplyMigrations(ctx, migrations[:7]))

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, strings.ReplaceAll(sql, "billing.", schema+"."), args...)
		require.NoError(t, err)
	}
	merchantID, customerID, productID, pspID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	recurringID, rentalID, durableID, subscriptionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'duration','active')`, merchantID)
	exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, merchantID, customerID)
	exec(`INSERT INTO billing.products(merchant_id,id,key,display_name,tier_rank,archived,entitlements_spec)
		VALUES($1,$2,'membership','Membership',0,false,'{"read":null}')`, merchantID, productID)
	exec(`INSERT INTO billing.psps(merchant_id,id,key,rail,environment,account_id)
		VALUES($1,$2::uuid,'stripe','stripe','test',$2::text)`, merchantID, pspID)
	for _, price := range []struct {
		id     uuid.UUID
		key    string
		hours  *int
		recurs bool
	}{
		{recurringID, "recurring", new(720), true},
		{rentalID, "rental", new(72), false},
		{durableID, "durable", nil, false},
	} {
		exec(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived,access_duration_hours,auto_renew)
			VALUES($1,$2,$3,$4,1000000,'USD',false,$5,$6)`, merchantID, price.id, productID, price.key, price.hours, price.recurs)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	trialEnd, paidEnd := start.Add(24*time.Hour), start.Add(744*time.Hour)
	exec(`INSERT INTO billing.subscriptions(merchant_id,id,customer_id,product_id,price_id,psp_id,status,rail,collection_policy,
		started_at,current_period_starts_at,current_period_ends_at)
		VALUES($1,$2,$3,$4,$5,$6,'active','stripe','provider',$7,$8,$9)`,
		merchantID, subscriptionID, customerID, productID, recurringID, pspID, start, trialEnd, paidEnd)
	firstGrant := uuid.New()
	for i, window := range [][2]time.Time{{start, trialEnd}, {trialEnd, paidEnd}} {
		id := firstGrant
		if i > 0 {
			id = uuid.New()
		}
		exec(`INSERT INTO billing.grants(merchant_id,id,customer_id,product_id,kind,source_type,source_id,event,spec_snapshot,starts_at,ends_at)
			VALUES($1,$2,$3,$4,'entitlement','subscription',$5,'grant','{"entitlements":["read"]}',$6,$7)`,
			merchantID, id, customerID, productID, subscriptionID.String(), window[0], window[1])
	}
	exec(`INSERT INTO billing.entitlements(merchant_id,id,customer_id,entitlement,source_type,source_id,grant_id,starts_at)
		VALUES($1,$2,$3,'read','subscription',$4,$5,$6)`, merchantID, uuid.New(), customerID, subscriptionID, firstGrant, start)

	require.NoError(t, migrator.ApplyMigrations(ctx, migrations[:8]))
	var billingHours, accessHours *int
	require.NoError(t, pool.QueryRow(ctx, "SELECT billing_interval_hours,access_duration_hours FROM "+schema+".prices WHERE id=$1", recurringID).Scan(&billingHours, &accessHours))
	require.Equal(t, new(720), billingHours)
	require.Equal(t, new(720), accessHours)
	for _, id := range []uuid.UUID{rentalID, durableID} {
		require.NoError(t, pool.QueryRow(ctx, "SELECT billing_interval_hours FROM "+schema+".prices WHERE id=$1", id).Scan(&billingHours))
		require.Nil(t, billingHours)
	}
	var snapshot int
	require.NoError(t, pool.QueryRow(ctx, "SELECT access_duration_hours_snapshot FROM "+schema+".subscriptions WHERE id=$1", subscriptionID).Scan(&snapshot))
	require.Equal(t, 720, snapshot)
	var projectedEnd, retainedTrialEnd time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT ends_at FROM "+schema+".entitlements WHERE source_id=$1", subscriptionID).Scan(&projectedEnd))
	require.Equal(t, paidEnd, projectedEnd.UTC())
	require.NoError(t, pool.QueryRow(ctx, "SELECT ends_at FROM "+schema+".grants WHERE id=$1", firstGrant).Scan(&retainedTrialEnd))
	require.Equal(t, trialEnd, retainedTrialEnd.UTC())

	for _, access := range []*int{new(72), nil} {
		exec(`INSERT INTO billing.prices(merchant_id,product_id,key,amount,currency,archived,billing_interval_hours,access_duration_hours)
			VALUES($1,$2,$3,1000000,'USD',false,720,$4)`, merchantID, productID, uuid.NewString(), access)
	}
	_, err = pool.Exec(ctx, "INSERT INTO "+schema+".prices(merchant_id,product_id,key,amount,currency,archived,billing_interval_hours) VALUES($1,$2,'zero',1,'USD',false,0)", merchantID, productID)
	require.ErrorContains(t, err, "prices_billing_interval_positive_check")
	_, err = pool.Exec(ctx, "UPDATE "+schema+".prices SET billing_interval_hours=24 WHERE id=$1", recurringID)
	require.ErrorContains(t, err, "immutable billing facts")
}
