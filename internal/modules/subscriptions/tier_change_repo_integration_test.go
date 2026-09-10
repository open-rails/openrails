//go:build integration

package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestReplaceForTierChangeCommitsBothSides(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	now := time.Date(2026, time.September, 10, 9, 30, 0, 0, time.UTC)
	productID, priceID, oldID := uuid.New(), uuid.New(), uuid.New()
	insertCatalogAndSub(ctx, t, database, now, 30, productID, priceID, oldID, uuid.NewString(), now, now.Add(30*24*time.Hour))

	repo := NewSubscriptionRepo(database)
	oldSub, err := repo.GetByID(ctx, oldID)
	require.NoError(t, err)
	newSub := replacementSubscription(oldSub, now)
	cancelType := models.CancelTypeUser
	oldSub.Status = models.StatusCancelled
	oldSub.CancelType = &cancelType
	oldSub.CancelledAt = &now
	oldSub.EndedAt = &now

	t.Cleanup(func() {
		_, _ = database.Pool().Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = ANY($1)", []uuid.UUID{oldID, newSub.ID})
		_, _ = database.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = database.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	require.NoError(t, repo.ReplaceForTierChange(ctx, oldSub, newSub, now))

	persistedOld, err := repo.GetByID(ctx, oldID)
	require.NoError(t, err)
	require.Equal(t, models.StatusCancelled, persistedOld.Status)
	persistedNew, err := repo.GetByID(ctx, newSub.ID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, persistedNew.Status)
	require.Equal(t, oldSub.CustomerID, persistedNew.CustomerID)
	require.Equal(t, oldSub.PriceID, persistedNew.PriceID)
}

func TestReplaceForTierChangeRollsBackOldSideWhenInsertFails(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	now := time.Date(2026, time.September, 10, 10, 0, 0, 0, time.UTC)
	productID, priceID, oldID := uuid.New(), uuid.New(), uuid.New()
	insertCatalogAndSub(ctx, t, database, now, 30, productID, priceID, oldID, uuid.NewString(), now, now.Add(30*24*time.Hour))

	repo := NewSubscriptionRepo(database)
	oldSub, err := repo.GetByID(ctx, oldID)
	require.NoError(t, err)
	newSub := replacementSubscription(oldSub, now)
	newSub.ProductID = uuid.New() // force the second statement's product FK to fail
	cancelType := models.CancelTypeUser
	oldSub.Status = models.StatusCancelled
	oldSub.CancelType = &cancelType
	oldSub.CancelledAt = &now
	oldSub.EndedAt = &now

	t.Cleanup(func() {
		_, _ = database.Pool().Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = ANY($1)", []uuid.UUID{oldID, newSub.ID})
		_, _ = database.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = database.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	require.Error(t, repo.ReplaceForTierChange(ctx, oldSub, newSub, now))

	persistedOld, err := repo.GetByID(ctx, oldID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, persistedOld.Status, "failed replacement must roll back the old subscription update")
	_, err = repo.GetByID(ctx, newSub.ID)
	require.Error(t, err, "failed replacement must not leave a new subscription")
}

func TestApplyLocalCancellationPersistsTerminalState(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	now := time.Date(2026, time.September, 10, 11, 0, 0, 0, time.UTC)
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	insertCatalogAndSub(ctx, t, database, now, 30, productID, priceID, subID, uuid.NewString(), now, now.Add(30*24*time.Hour))
	lastRetry, nextRetry, graceEnd := now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour)
	_, err := database.Pool().Exec(ctx, `UPDATE openrails.subscriptions
		SET retry_attempts=2, last_retry_at=$2, next_retry_at=$3, grace_ends_at=$4
		WHERE id=$1`, subID, lastRetry, nextRetry, graceEnd)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = database.Pool().Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = database.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = database.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	repo := NewSubscriptionRepo(database)
	sub, err := repo.GetByID(ctx, subID)
	require.NoError(t, err)
	feedback := "requested in account settings"
	lifecycle := newLifecycleForTest(database)
	lifecycle.SetClock(clockwork.NewFakeClockAt(now))
	require.NoError(t, lifecycle.ApplyLocalCancellation(ctx, database, sub, LocalCancellation{
		EndedAt:    now,
		CancelType: models.CancelTypeUser,
		Feedback:   &feedback,
		RevokeAsOf: now,
	}))

	persisted, err := repo.GetByID(ctx, subID)
	require.NoError(t, err)
	require.Equal(t, models.StatusCancelled, persisted.Status)
	require.NotNil(t, persisted.CancelType)
	require.Equal(t, models.CancelTypeUser, *persisted.CancelType)
	require.NotNil(t, persisted.CancelledAt)
	require.Equal(t, now, persisted.CancelledAt.UTC())
	require.NotNil(t, persisted.EndedAt)
	require.Equal(t, now, persisted.EndedAt.UTC())
	require.Equal(t, &feedback, persisted.CancelFeedback)
	require.Nil(t, persisted.RetryAttempts)
	require.Nil(t, persisted.LastRetryAt)
	require.Nil(t, persisted.NextRetryAt)
	require.Nil(t, persisted.GraceEndsAt)
}

func replacementSubscription(oldSub *models.Subscription, now time.Time) *models.Subscription {
	newSub := *oldSub
	newSub.ID = uuid.New()
	newSub.Status = models.StatusActive
	newSub.RailSubscriptionID = "replacement-" + uuid.NewString()
	newSub.StartedAt = now
	newSub.CreatedAt = now
	newSub.UpdatedAt = now
	newSub.EndedAt = nil
	newSub.CancelType = nil
	newSub.CancelledAt = nil
	newSub.CancelFeedback = nil
	return &newSub
}
