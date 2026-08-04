//go:build integration

package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#878: the host-lifecycle feed carries SHUTOFF instructions, so its isolation
// is the same class of requirement as the settlement feed's (#827) — and it is
// built the same way from the start rather than retrofitted: the control-plane
// functions scope by explicit predicate under MerchantTx, and the table itself
// is fail-closed RLS for the openrails_app (NOBYPASSRLS) role.
//
// A leak here is a leak of who is failing to pay whom; a cross-merchant ack
// would silently drop a real customer's shutoff or restore.
func TestHostLifecycleEventsCrossMerchantIsolation(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	super := dbtest.SharedSuperuserPGXPool(t)

	appPool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	cfg := &config.Config{
		Env:  "test",
		DB:   &config.DBConfig{},
		Auth: &config.AuthConfig{Issuer: "https://openrails.test", MintDisabled: true},
	}
	cp, err := New(ctx, cfg, super)
	require.NoError(t, err)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	mA, mB := uuid.New(), uuid.New()
	custA, custB := uuid.New(), uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := super.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	exec(`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active'), ($3, $4, 'active')`,
		mA, "hleiso-a-"+suffix, mB, "hleiso-b-"+suffix)
	exec(`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2), ($3, $4)`, custA, mA, custB, mB)
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(), `DELETE FROM openrails.host_lifecycle_events WHERE merchant_id = ANY($1)`, []uuid.UUID{mA, mB})
		_, _ = super.Exec(context.Background(), `DELETE FROM openrails.customers WHERE id = ANY($1)`, []uuid.UUID{custA, custB})
		_, _ = super.Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id = ANY($1)`, []uuid.UUID{mA, mB})
	})

	// Enqueue as openrails_app under each merchant's GUC, so the insert has to
	// pass the fail-closed WITH CHECK rather than ride a privileged pool.
	enqueue := func(mid, subject uuid.UUID, dedupe string) {
		t.Helper()
		tx, err := appPool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, mid.String())
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `INSERT INTO openrails.host_lifecycle_events
			(merchant_id, event_type, subject_type, subject_id, currency, data, dedupe_key)
			VALUES ($1, 'delinquency.entered', 'customer', $2, 'USD', '{"to_state":"delinquent"}'::jsonb, $3)`,
			mid, subject, dedupe)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
	}
	enqueue(mA, custA, "delinquency:"+custA.String()+":USD:1")
	enqueue(mB, custB, "delinquency:"+custB.String()+":USD:1")

	idA, idB := merchant.ID(mA), merchant.ID(mB)

	listA, err := cp.ListPendingHostLifecycleEvents(ctx, idA, 10)
	require.NoError(t, err)
	require.Len(t, listA, 1)
	require.Equal(t, custA, listA[0].SubjectID)
	require.Equal(t, idA, listA[0].MerchantID)
	require.Equal(t, "delinquency.entered", listA[0].EventType)
	require.Equal(t, "USD", listA[0].Currency)
	require.Equal(t, "delinquent", listA[0].Data["to_state"])

	listB, err := cp.ListPendingHostLifecycleEvents(ctx, idB, 10)
	require.NoError(t, err)
	require.Len(t, listB, 1)
	require.Equal(t, custB, listB[0].SubjectID)

	// A cannot ack B's event, and B's feed is undisturbed by the attempt.
	require.ErrorIs(t, cp.AcknowledgeHostLifecycleEvent(ctx, idA, listB[0].ID), ErrHostLifecycleEventNotFound)
	listB, err = cp.ListPendingHostLifecycleEvents(ctx, idB, 10)
	require.NoError(t, err)
	require.Len(t, listB, 1, "cross-merchant ack must not deliver B's event")

	// A zero merchant id is refused outright rather than defaulting to anyone.
	_, err = cp.ListPendingHostLifecycleEvents(ctx, merchant.ID{}, 10)
	require.Error(t, err)
	require.Error(t, cp.AcknowledgeHostLifecycleEvent(ctx, merchant.ID{}, listB[0].ID))

	// RLS proof on the NOBYPASSRLS app role.
	appTx := func(mid *uuid.UUID, fn func(tx gen.DBTX)) {
		t.Helper()
		tx, err := appPool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		if mid != nil {
			_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, mid.String())
			require.NoError(t, err)
		}
		fn(tx)
		require.NoError(t, tx.Commit(ctx))
	}
	appTx(&mA, func(tx gen.DBTX) {
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.host_lifecycle_events WHERE merchant_id = $1`, mB).Scan(&n))
		require.Zero(t, n, "app role under A's GUC must not see B's events")
		tag, err := tx.Exec(ctx, `UPDATE openrails.host_lifecycle_events SET delivered_at = now() WHERE id = $1`, listB[0].ID)
		require.NoError(t, err)
		require.Zero(t, tag.RowsAffected(), "app role under A's GUC must not ack B's event")
	})
	appTx(nil, func(tx gen.DBTX) {
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.host_lifecycle_events`).Scan(&n))
		require.Zero(t, n, "no merchant GUC must fail closed")
	})

	// B acks its own event; ack is idempotent; the feed drains.
	require.NoError(t, cp.AcknowledgeHostLifecycleEvent(ctx, idB, listB[0].ID))
	require.NoError(t, cp.AcknowledgeHostLifecycleEvent(ctx, idB, listB[0].ID))
	listB, err = cp.ListPendingHostLifecycleEvents(ctx, idB, 10)
	require.NoError(t, err)
	require.Empty(t, listB)

	listA, err = cp.ListPendingHostLifecycleEvents(ctx, idA, 10)
	require.NoError(t, err)
	require.Len(t, listA, 1, "acking B's event must not drain A's feed")

	// Re-emitting the SAME transition is a no-op, not a second instruction to
	// shut a customer off.
	enqueueDup := func(mid, subject uuid.UUID, dedupe string) int64 {
		t.Helper()
		var affected int64
		appTx(&mid, func(tx gen.DBTX) {
			tag, err := tx.Exec(ctx, `INSERT INTO openrails.host_lifecycle_events
				(merchant_id, event_type, subject_type, subject_id, currency, data, dedupe_key)
				VALUES ($1, 'delinquency.entered', 'customer', $2, 'USD', '{}'::jsonb, $3)
				ON CONFLICT (merchant_id, dedupe_key) DO NOTHING`, mid, subject, dedupe)
			require.NoError(t, err)
			affected = tag.RowsAffected()
		})
		return affected
	}
	require.Zero(t, enqueueDup(mA, custA, "delinquency:"+custA.String()+":USD:1"))
	listA, err = cp.ListPendingHostLifecycleEvents(ctx, idA, 10)
	require.NoError(t, err)
	require.Len(t, listA, 1)

	// The same dedupe key under a DIFFERENT merchant is a different event: the
	// unique is merchant-scoped, so one merchant can never block another's insert.
	require.EqualValues(t, 1, enqueueDup(mB, custB, "delinquency:"+custA.String()+":USD:1"))
}
