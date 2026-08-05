//go:build integration

package riverjobs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

// or#837: the hourly retention sweep must cost ACTIVITY, not the merchant
// directory.
//
// The old worker called ListActiveMerchantIDs — every active merchant, no
// cursor, no LIMIT — and opened six transactions for each one to discover
// nothing to delete. This test is the wall: seed N merchants, give only K of
// them a row past a retention cutoff, and assert the pass VISITS exactly those
// K. Asserting on deleted rows alone cannot catch the regression, because a
// worker that walks all N and deletes nothing for N-K of them deletes exactly
// the same rows.
func TestCleanupVisitsOnlyMerchantsWithDueWork(t *testing.T) {
	ctx := context.Background()
	super := dbtest.SharedSuperuserPGXPool(t)
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	resetSweepCursor(t, ctx, super)

	const (
		idle = 8 // merchants with nothing due
		due  = 3 // merchants holding one expired webhook dedup mark
	)
	var withWork []uuid.UUID
	for i := 0; i < idle+due; i++ {
		mid := seedScalingMerchant(t, ctx, super)
		// Every merchant gets a RECENT mark, so "has rows" is not what the
		// queue keys on — only "has rows past the cutoff" is.
		insertWebhookMark(t, ctx, super, mid, time.Now().UTC().Add(-24*time.Hour))
		if i < due {
			insertWebhookMark(t, ctx, super, mid, time.Now().UTC().Add(-120*24*time.Hour))
			withWork = append(withWork, mid)
		}
	}

	w := CleanupExpiredDataWorker{DB: dbi, Config: DefaultCleanupConfig()}
	visited, result, err := w.sweepPass(ctx)
	require.NoError(t, err)

	require.Subset(t, visited, withWork, "every merchant with due work must be visited")
	for _, mid := range idleMerchantsOf(withWork, visited) {
		t.Fatalf("merchant %s had no due retention work but the pass visited it", mid)
	}
	require.GreaterOrEqual(t, result.WebhookEvents, int64(due))

	// And the deletes are right: the aged marks are gone, the recent ones stay.
	for _, mid := range withWork {
		require.Equal(t, 1, countWebhookMarks(t, ctx, super, mid),
			"only the mark past the retention window may be deleted")
	}
}

// A capped pass must be BOUNDED and RESUMABLE. A LIMIT alone would sweep the
// same head every hour and starve the tail forever — which is exactly why the
// issue called a bare LIMIT the wrong fix. The cursor is what makes the cap
// safe, so this asserts the cursor itself, not just that rows eventually go.
func TestCleanupCappedPassResumesWhereItLeftOff(t *testing.T) {
	ctx := context.Background()
	super := dbtest.SharedSuperuserPGXPool(t)
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	resetSweepCursor(t, ctx, super)

	// Six merchants with due work, two per pass: the tail is only reachable if
	// the cursor advances.
	const total = 6
	work := map[uuid.UUID]bool{}
	for i := 0; i < total; i++ {
		mid := seedScalingMerchant(t, ctx, super)
		insertWebhookMark(t, ctx, super, mid, time.Now().UTC().Add(-120*24*time.Hour))
		work[mid] = true
	}

	w := CleanupExpiredDataWorker{DB: dbi, Config: DefaultCleanupConfig(), MerchantBatch: 2}

	first, _, err := w.sweepPass(ctx)
	require.NoError(t, err)
	require.Len(t, first, 2, "the pass must stop at the cap")

	cursor := readSweepCursor(t, ctx, super)
	require.NotNil(t, cursor, "a capped pass must leave a resume point")
	require.Equal(t, first[len(first)-1], *cursor, "the cursor is the last merchant handled")

	// The queue is ordered by merchant id, so everything still holding due work
	// sorts after that cursor — and the next pass must start there, not at the
	// head it already swept.
	second, _, err := w.sweepPass(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, second, "the tail must be reachable")
	for _, mid := range second {
		require.Greater(t, mid.String(), cursor.String(),
			"the pass re-served an already-swept merchant instead of resuming")
	}

	// And every merchant is reached across passes — the tail never starves.
	seen := map[uuid.UUID]bool{}
	for _, mid := range append(append([]uuid.UUID{}, first...), second...) {
		seen[mid] = true
	}
	for pass := 0; pass < 20 && !coversAll(seen, work); pass++ {
		visited, _, err := w.sweepPass(ctx)
		require.NoError(t, err)
		for _, mid := range visited {
			seen[mid] = true
		}
	}
	for mid := range work {
		require.True(t, seen[mid], "merchant %s never got a pass — the cap starved the tail", mid)
		require.Zero(t, countWebhookMarks(t, ctx, super, mid), "its expired mark must be gone")
	}
}

func coversAll(seen map[uuid.UUID]bool, work map[uuid.UUID]bool) bool {
	for mid := range work {
		if !seen[mid] {
			return false
		}
	}
	return true
}

// idleMerchantsOf returns the visited merchants that are NOT in the expected
// due-work set — the ones a directory walk would have swept for nothing. Only
// merchants seeded by this test are considered: the shared database carries
// fixtures from every other package.
func idleMerchantsOf(expected, visited []uuid.UUID) []uuid.UUID {
	want := map[uuid.UUID]bool{}
	for _, m := range expected {
		want[m] = true
	}
	var out []uuid.UUID
	for _, m := range visited {
		if scalingSeeded[m] && !want[m] {
			out = append(out, m)
		}
	}
	return out
}

// scalingSeeded tracks the merchants these tests created, so assertions can
// ignore the shared database's other fixtures.
var scalingSeeded = map[uuid.UUID]bool{}

func seedScalingMerchant(t *testing.T, ctx context.Context, super *pgxpool.Pool) uuid.UUID {
	t.Helper()
	mid := uuid.New()
	_, err := super.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		mid, fmt.Sprintf("or837-%s", mid.String()[:8]))
	require.NoError(t, err)
	scalingSeeded[mid] = true
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = super.Exec(bg, "DELETE FROM openrails.webhook_events WHERE merchant_id = $1", mid)
		_, _ = super.Exec(bg, "DELETE FROM openrails.merchants WHERE id = $1", mid)
		delete(scalingSeeded, mid)
	})
	return mid
}

func insertWebhookMark(t *testing.T, ctx context.Context, super *pgxpool.Pool, mid uuid.UUID, at time.Time) {
	t.Helper()
	_, err := super.Exec(ctx,
		`INSERT INTO openrails.webhook_events (merchant_id, op, event_id, created_at, completed_at)
		 VALUES ($1, 'webhook.ccbill.TestEvent', $2, $3, $3)`,
		mid, "or837_"+uuid.NewString(), at)
	require.NoError(t, err)
}

func countWebhookMarks(t *testing.T, ctx context.Context, super *pgxpool.Pool, mid uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, super.QueryRow(ctx,
		"SELECT count(*) FROM openrails.webhook_events WHERE merchant_id = $1", mid).Scan(&n))
	return n
}

func readSweepCursor(t *testing.T, ctx context.Context, super *pgxpool.Pool) *uuid.UUID {
	t.Helper()
	var cur *uuid.UUID
	require.NoError(t, super.QueryRow(ctx,
		"SELECT cursor_merchant_id FROM openrails.worker_sweep_cursors WHERE worker_kind = $1",
		KindCleanupExpiredData).Scan(&cur))
	return cur
}

// The cursor is deployment-global state, so a test that asserts on paging must
// start from a known position.
func resetSweepCursor(t *testing.T, ctx context.Context, super *pgxpool.Pool) {
	t.Helper()
	_, err := super.Exec(ctx,
		"DELETE FROM openrails.worker_sweep_cursors WHERE worker_kind = $1", KindCleanupExpiredData)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(),
			"DELETE FROM openrails.worker_sweep_cursors WHERE worker_kind = $1", KindCleanupExpiredData)
	})
}
