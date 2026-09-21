//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

// A request's cancellation can close its pinned connection (pgx closes a
// connection whose BEGIN is interrupted). A detached completion write must still
// land under the same merchant scope, on a one-connection pool the request
// still occupies, and must leave no merchant scope behind on release.
func TestDetachedWriteRepinsAConnectionTheCallerClosed(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	seed := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(ctx, t, seed)
	psp := dbtest.EnsureTestPSP(ctx, t, seed, dbtest.TestMerchantID.UUID(), "nmi")

	app := dbtest.OpenOneConnAppDB(t)
	visible := func(ctx context.Context) (int, error) {
		var n int
		err := app.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM billing.psps WHERE id = $1`, psp).Scan(&n)
		return n, err
	}

	pinned, release, err := app.WithMerchantConn(ctx)
	require.NoError(t, err)
	defer release()
	request, cancel := context.WithCancel(pinned)
	defer cancel()
	n, err := visible(request)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Live pin: reused, never a second acquire (which this pool cannot serve).
	acquires := app.Pool().Stat().AcquireCount()
	live, liveCancel := db.DetachedWriteContext(request, 5*time.Second)
	n, err = visible(live)
	liveCancel()
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, acquires, app.Pool().Stat().AcquireCount())

	cancel()
	require.Error(t, app.MerchantTx(request, func(context.Context, pgx.Tx) error { return nil }))
	_, err = visible(pinned)
	require.Error(t, err, "an ordinary query never re-pins a closed connection")

	detached, detachedCancel := db.DetachedWriteContext(request, 5*time.Second)
	defer detachedCancel()
	started := time.Now()
	n, err = visible(detached)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the replacement connection carries the same merchant scope")
	require.Less(t, time.Since(started), 2*time.Second, "the closed connection's slot is freed before the re-acquire")
	require.EqualValues(t, 1, app.Pool().Stat().TotalConns())

	release()
	require.Zero(t, app.Pool().Stat().AcquiredConns())
	var remainingScope string
	require.NoError(t, app.Pool().QueryRow(ctx, `SELECT COALESCE(current_setting('app.merchant_id', true), '')`).Scan(&remainingScope))
	require.Empty(t, remainingScope, "the released replacement carries no merchant scope")
}
