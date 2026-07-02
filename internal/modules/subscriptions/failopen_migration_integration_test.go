//go:build integration

package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

// #691 migration 063: existing LIVE windows sourced from auto-renew-priced,
// non-terminal subs become STANDING (end_at NULL); only the LATEST live window
// per timeline; cancelled runway, bounded prices, one-off windows and
// conflicting-future-window shapes are untouched; the GIST no-overlap
// constraint holds throughout. The migration already ran at bootstrap, so the
// test seeds pre-inversion shapes and re-executes the (idempotent) statement.
func TestFailOpenMigration_ConvertsLiveAutoRenewWindows(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	merchantID := dbtest.TestMerchantID.UUID()
	sfx := uuid.NewString()[:8]

	migrationSQL, err := postgresmigrations.FS.ReadFile("063_failopen_standing_entitlements.up.sql")
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	exec := func(sql string, args ...any) {
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}

	type seeded struct {
		ent      uuid.UUID
		customer uuid.UUID
	}
	var cleanupCustomers []uuid.UUID
	var cleanupProducts []uuid.UUID

	// seedSub creates (product, price, sub) for a fresh customer and returns ids.
	seedSub := func(key, status string, autoRenew bool, periodEnd time.Time) (uuid.UUID, uuid.UUID) {
		prod, price, sub := uuid.New(), uuid.New(), uuid.New()
		cust := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
		cleanupCustomers = append(cleanupCustomers, cust)
		cleanupProducts = append(cleanupProducts, prod)
		exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`,
			prod, "mig691-"+key+"-"+sfx, merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES ($1,$2,5000000,'usd',720,$3,$4)`,
			price, prod, autoRenew, merchantID)
		if status == "cancelled" {
			exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type,ended_at)
			      VALUES ($1,$2,$3,$4,$5,'cancelled','nmi',$6,$7,$7,$8,$7,'user',$8)`, sub, merchantID, cust, prod, price, "mig-"+key+"-"+sfx, now.Add(-30*24*time.Hour), periodEnd)
		} else {
			exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at)
			      VALUES ($1,$2,$3,$4,$5,$6,'nmi',$7,$8,$8,$9)`, sub, merchantID, cust, prod, price, status, "mig-"+key+"-"+sfx, now.Add(-30*24*time.Hour), periodEnd)
		}
		return sub, cust
	}
	seedWindow := func(cust, source uuid.UUID, feat string, start time.Time, end *time.Time) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO openrails.entitlements (id,merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type)
		      VALUES ($1,$2,$3,$4,$5,$6,$7,'subscription')`, id, merchantID, cust, feat, start, end, source)
		return id
	}

	futureEnd := now.Add(15 * 24 * time.Hour)
	pastEnd := now.Add(-15 * 24 * time.Hour)

	// A) active auto-renew: older lapsed window + latest live window.
	subA, custA := seedSub("a", "active", true, futureEnd)
	featA := "mig-a-" + sfx
	oldA := seedWindow(custA, subA, featA, now.Add(-60*24*time.Hour), &pastEnd)
	liveA := seedWindow(custA, subA, featA, pastEnd, &futureEnd)

	// B) unknown auto-renew with a live window: converted too (non-terminal).
	subB, custB := seedSub("b", "unknown", true, futureEnd)
	featB := "mig-b-" + sfx
	liveB := seedWindow(custB, subB, featB, now.Add(-10*24*time.Hour), &futureEnd)

	// C) CANCELLED auto-renew with runway: untouched (bounded to paid-through).
	subC, custC := seedSub("c", "cancelled", true, futureEnd)
	featC := "mig-c-" + sfx
	runwayC := seedWindow(custC, subC, featC, now.Add(-10*24*time.Hour), &futureEnd)

	// D) active NON-auto-renew (bounded rental price): untouched.
	subD, custD := seedSub("d", "active", false, futureEnd)
	featD := "mig-d-" + sfx
	boundedD := seedWindow(custD, subD, featD, now.Add(-10*24*time.Hour), &futureEnd)

	// E) active auto-renew whose window is followed by a conflicting future
	// one-off window on the same timeline: conversion skipped (GIST safety).
	subE, custE := seedSub("e", "active", true, futureEnd)
	featE := "mig-e-" + sfx
	skippedE := seedWindow(custE, subE, featE, now.Add(-10*24*time.Hour), &futureEnd)
	futureStart := futureEnd
	futureFuture := futureEnd.Add(10 * 24 * time.Hour)
	oneOffE := uuid.New()
	exec(`INSERT INTO openrails.entitlements (id,merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type)
	      VALUES ($1,$2,$3,$4,$5,$6,$7,'one_off')`, oneOffE, merchantID, custE, featE, futureStart, futureFuture, uuid.New())

	t.Cleanup(func() {
		for _, c := range cleanupCustomers {
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.entitlements WHERE customer_id=$1`, c)
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.subscriptions WHERE customer_id=$1`, c)
		}
		for _, p := range cleanupProducts {
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE product_id=$1`, p)
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, p)
		}
	})

	// Run the migration statement (idempotent re-run of 063) — GIST must hold.
	_, err = pool.Exec(ctx, string(migrationSQL))
	require.NoError(t, err, "migration must apply without tripping the no-overlap constraint")

	endOf := func(id uuid.UUID) *time.Time {
		var e *time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT end_at FROM openrails.entitlements WHERE id=$1`, id).Scan(&e))
		return e
	}

	require.Nil(t, endOf(liveA), "A: latest live auto-renew window converted to standing")
	require.NotNil(t, endOf(oldA), "A: older lapsed window is history, untouched")
	require.Nil(t, endOf(liveB), "B: unknown (non-terminal) auto-renew window converted")
	require.NotNil(t, endOf(runwayC), "C: cancelled runway stays bounded to paid-through")
	require.WithinDuration(t, futureEnd, *endOf(runwayC), time.Second)
	require.NotNil(t, endOf(boundedD), "D: non-auto-renew price stays bounded")
	require.NotNil(t, endOf(skippedE), "E: conflicting-future-window shape skipped (GIST safety)")
	require.NotNil(t, endOf(oneOffE), "E: one-off window untouched")

	// Idempotent: a second run changes nothing further and still satisfies GIST.
	_, err = pool.Exec(ctx, string(migrationSQL))
	require.NoError(t, err)
	require.Nil(t, endOf(liveA))
	require.NotNil(t, endOf(skippedE))
}
