//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// #678: the retention sweep deletes completed webhook dedup marks older than
// the retention window and never touches recent ones.
func TestCleanupWebhookEventsRetention(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	ctx := context.Background()
	pool := dbi.Pool()

	now := time.Now().UTC()
	oldID := "evt_retention_old_" + uuid.NewString()
	recentID := "evt_retention_recent_" + uuid.NewString()
	insert := func(eventID string, completedAt time.Time) {
		_, err := pool.Exec(ctx,
			`INSERT INTO openrails.webhook_events (merchant_id, rail, op, event_id, created_at, completed_at)
			 VALUES ($1, 'ccbill', 'webhook.ccbill.TestEvent', $2, $3, $3)`,
			dbtest.TestMerchantID.UUID(), eventID, completedAt)
		require.NoError(t, err)
	}
	insert(oldID, now.Add(-91*24*time.Hour))
	insert(recentID, now.Add(-24*time.Hour))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.webhook_events WHERE event_id IN ($1, $2)", oldID, recentID)
	})

	w := CleanupExpiredDataWorker{DB: dbi}
	deleted, err := w.cleanupWebhookEvents(ctx, now, 90*24*time.Hour)
	require.NoError(t, err)
	require.GreaterOrEqual(t, deleted, int64(1))

	remaining := func(eventID string) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT count(*) FROM openrails.webhook_events WHERE event_id = $1", eventID).Scan(&n))
		return n
	}
	require.Equal(t, 0, remaining(oldID), "expired mark must be deleted")
	require.Equal(t, 1, remaining(recentID), "recent mark must survive")

	// Zero/unset retention falls back to the 90d default rather than deleting everything.
	deleted, err = w.cleanupWebhookEvents(ctx, now, 0)
	require.NoError(t, err)
	require.Equal(t, 1, remaining(recentID), "default retention must not delete recent marks")
}
