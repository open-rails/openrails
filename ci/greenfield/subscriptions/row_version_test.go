//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// A full-row write from a stale image is refused (#1102): the retry schedule
// another writer set meanwhile, here by raw SQL, survives.
func TestStaleSubscriptionImageNeverReverts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	id := uuid.MustParse(subUUID(e.sub))
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	retry := w.clock.Now().Add(48 * time.Hour).UTC().Truncate(time.Microsecond)
	err = d.RunInMerchantScope(t.Context(), w.client[embedded].MerchantID(), "stale image", func(ctx context.Context) error {
		repo := subscriptions.NewSubscriptionRepo(d)
		stale, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		_, err = w.pool.Exec(ctx, w.q(`UPDATE openrails.subscriptions SET next_retry_at = $2 WHERE id = $1`), id, retry)
		require.NoError(t, err)
		email := "stale@example.test"
		stale.UserEmail = &email
		require.ErrorIs(t, repo.UpdateAt(ctx, stale, w.clock.Now()), subscriptions.ErrSubscriptionMoved)

		fresh, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		fresh.UserEmail = &email
		require.NoError(t, repo.UpdateAt(ctx, fresh, w.clock.Now()), "a current image writes")
		return nil
	})
	require.NoError(t, err)
	sub := w.subscription(embedded, e.sub)
	require.NotNil(t, sub.NextRetryAt)
	require.True(t, sub.NextRetryAt.Equal(retry), "the other writer's retry schedule survives")
}
