//go:build integration

package embed

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/embedded"
)

// TestInProcessClientHasNoHiddenTimeout proves #767: an in-process SDK call
// through Runtime.Client is bounded ONLY by the caller's ctx, not a hidden 2s
// per-request deadline. Before the fix, Runtime.Client built the in-process
// remote without openrails.WithTimeout(0), so every call inherited remote.go's
// default 2s context deadline regardless of the caller's own ctx.
//
// No mocks: a real competing pgx transaction holds a row lock on the
// merchant's config row (the same row SetMerchantSettings's UPSERT must lock)
// for ~3s. A generous (10s) caller ctx must let the call block on the real
// lock and still succeed — which is impossible if a hidden 2s deadline fires
// first.
func TestInProcessClientHasNoHiddenTimeout(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	// The competing locker must run in the SAME merchant scope as the call it is
	// meant to block. The default integration DSN is the RLS-enforcing
	// openrails_app role, so a FOR UPDATE on an unpinned connection matches ZERO
	// rows — no error, nothing locked, and the timing assertion below would pass
	// on an unblocked call. dbtest.SharedMerchantPool pins app.merchant_id on
	// every acquire, exactly as MerchantTx does for the call under test.
	lockPool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())

	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}
	rt, err := New(ctx, Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	rt.emb.App().Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)

	c := rt.Client()

	// Seed the merchant_configurations row so the competing FOR UPDATE below
	// has a row to lock, and so the timed call below is an UPDATE (which needs
	// the row lock) rather than a lock-free first INSERT.
	require.NoError(t, c.SetMerchantSettings(ctx, openrails.MerchantSettings{}))

	lockTx, err := lockPool.Begin(ctx)
	require.NoError(t, err)
	tag, err := lockTx.Exec(ctx,
		`SELECT 1 FROM openrails.merchant_configurations WHERE merchant_id = $1 FOR UPDATE`,
		dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(),
		"the competing tx locked no row — the timing assertion below would be vacuous")

	const holdFor = 3 * time.Second
	released := make(chan struct{})
	go func() {
		time.Sleep(holdFor)
		_ = lockTx.Commit(context.Background())
		close(released)
	}()
	t.Cleanup(func() { <-released })

	// Generous caller ctx: comfortably longer than the row-lock hold, and MUCH
	// longer than the pre-fix hidden 2s deadline this test is guarding against.
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	start := time.Now()
	err = c.SetMerchantSettings(callCtx, openrails.MerchantSettings{})
	elapsed := time.Since(start)

	t.Logf("in-process SetMerchantSettings blocked on the row lock for %s (hold %s)", elapsed, holdFor)

	require.NoError(t, err, "in-process call must succeed once the real row lock releases, "+
		"bounded only by the caller ctx (not a hidden per-call deadline)")
	require.GreaterOrEqual(t, elapsed, holdFor,
		"call returned before the row lock was released — it did not actually block on the lock")
}
