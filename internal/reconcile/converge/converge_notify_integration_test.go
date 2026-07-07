//go:build integration

package converge

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #789 NOTIFY pass: a customer whose last entitlement window closed gets
// exactly one premium_ended/access_ended notification row (emailed_at NULL);
// re-running converge never duplicates it; a live replacement window or a
// pre-existing premium_ended (a transition-site email) suppresses it.
func TestConvergeNotify_AccessEnded(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]
	now := time.Now().UTC()

	type fixture struct {
		cust uuid.UUID
		ents []uuid.UUID
	}
	var lapsed, replaced, alreadyTold fixture

	seedWindow := func(ctx context.Context, f *fixture, entName string, start time.Time, end *time.Time) {
		id := uuid.New()
		_, err := appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.entitlements (id, merchant_id, customer_id, entitlement, start_at, end_at, source_id, source_type)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,'admin')`,
			id, merchantID, f.cust, entName, start, end, uuid.New())
		require.NoError(t, err)
		f.ents = append(f.ents, id)
	}

	closedAt := now.Add(-2 * 24 * time.Hour)
	feat := "notify-feat-" + sfx
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		lapsed.cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		replaced.cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		alreadyTold.cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())

		// (a) one window, ended 2 days ago, no replacement.
		seedWindow(ctx, &lapsed, feat, now.Add(-30*24*time.Hour), &closedAt)
		// (b) closed window BUT a live replacement for the same entitlement.
		seedWindow(ctx, &replaced, feat, now.Add(-30*24*time.Hour), &closedAt)
		seedWindow(ctx, &replaced, feat, now.Add(-24*time.Hour), nil)
		// (c) closed window + premium_ended notification created after the close
		// (the dunning/transition-site email already went out).
		seedWindow(ctx, &alreadyTold, feat, now.Add(-30*24*time.Hour), &closedAt)
		_, err := appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.notification_queue (id, merchant_id, customer_id, event_type, data)
			 VALUES ($1,$2,$3,'premium_ended','{"reason":"expired"}'::jsonb)`,
			uuid.New(), merchantID, alreadyTold.cust)
		require.NoError(t, err)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, f := range []fixture{lapsed, replaced, alreadyTold} {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.notification_queue WHERE merchant_id=$1 AND customer_id=$2`, merchantID, f.cust)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE id=ANY($1)`, f.ents)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "customer:"+f.cust.String())
			}
			return nil
		})
	})

	countPremiumEnded := func(ctx context.Context, cust uuid.UUID) int {
		t.Helper()
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.notification_queue WHERE merchant_id=$1 AND customer_id=$2 AND event_type='premium_ended'`,
			merchantID, cust).Scan(&n))
		return n
	}
	convergeCustomer := func(ctx context.Context, cust uuid.UUID) {
		t.Helper()
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
	}

	// (a) exactly one access_ended row, undelivered; re-run dedupes.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		convergeCustomer(ctx, lapsed.cust)
		require.Equal(t, 1, countPremiumEnded(ctx, lapsed.cust), "one premium_ended row created")

		var reason, endedAt, source string
		var emailedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT data->>'reason', data->>'ended_at', data->>'source', emailed_at FROM openrails.notification_queue
			 WHERE merchant_id=$1 AND customer_id=$2 AND event_type='premium_ended'`,
			merchantID, lapsed.cust).Scan(&reason, &endedAt, &source, &emailedAt))
		require.Equal(t, "access_ended", reason)
		require.Equal(t, "converge_notify", source)
		parsed, err := time.Parse(time.RFC3339, endedAt)
		require.NoError(t, err)
		require.WithinDuration(t, closedAt, parsed, time.Second)
		require.Nil(t, emailedAt, "delivery belongs to the sweep — row starts undelivered")

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='notify.access_ended.missing' AND subject_key=$2`,
			merchantID, "customer:"+lapsed.cust.String()).Scan(&status))
		require.Equal(t, "auto_fixed", status)

		convergeCustomer(ctx, lapsed.cust)
		convergeCustomer(ctx, lapsed.cust)
		require.Equal(t, 1, countPremiumEnded(ctx, lapsed.cust), "re-running converge never duplicates")
		return nil
	}))

	// (b) a live replacement window suppresses the notification entirely.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		convergeCustomer(ctx, replaced.cust)
		require.Equal(t, 0, countPremiumEnded(ctx, replaced.cust), "live window ⇒ access did not end")
		return nil
	}))

	// (c) a premium_ended row at/after the close (transition-site email) dedupes.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		convergeCustomer(ctx, alreadyTold.cust)
		require.Equal(t, 1, countPremiumEnded(ctx, alreadyTold.cust), "only the pre-existing dunning notification remains")
		var reason string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT data->>'reason' FROM openrails.notification_queue WHERE merchant_id=$1 AND customer_id=$2 AND event_type='premium_ended'`,
			merchantID, alreadyTold.cust).Scan(&reason))
		require.Equal(t, "expired", reason, "no access_ended row was added")
		return nil
	}))
}
