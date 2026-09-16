//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestWorkerStateConcurrentHealthAndCursorPreserveFields(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	q := gen.New(pool)
	kind := "test.state." + uuid.NewString()
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM openrails.worker_state WHERE worker_kind=$1`, kind) })
	cursor := uuid.New()
	period := int64(60)
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := make(chan struct{})
	results := make(chan error, 3)
	go func() {
		<-start
		results <- q.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{WorkerKind: kind, CursorMerchantID: &cursor})
	}()
	go func() {
		<-start
		results <- q.RecordWorkerSuccess(ctx, gen.RecordWorkerSuccessParams{WorkerKind: kind, Now: now})
	}()
	go func() {
		<-start
		results <- q.SeedWorkerHealth(ctx, gen.SeedWorkerHealthParams{WorkerKind: kind, ExpectedPeriodSeconds: &period})
	}()
	close(start)
	for range 3 {
		require.NoError(t, <-results)
	}
	// Reconstruct the reader as a restarted process would, then mutate the
	// opposite half of the row and verify both owners retain their state.
	restarted := gen.New(pool)
	failure := "failure after restart"
	stored, err := restarted.GetSweepCursor(ctx, kind)
	require.NoError(t, err)
	require.Equal(t, &cursor, stored)
	require.NoError(t, restarted.RecordWorkerFailure(ctx, gen.RecordWorkerFailureParams{WorkerKind: kind, Now: now, LastError: &failure}))
	require.NoError(t, restarted.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{WorkerKind: kind}))
	rows, err := restarted.ListWorkerHealth(ctx)
	require.NoError(t, err)
	for _, row := range rows {
		if row.WorkerKind == kind {
			require.Nil(t, row.CursorMerchantID)
			require.NotNil(t, row.CursorUpdatedAt)
			require.Equal(t, &now, row.LastSuccessAt)
			require.Equal(t, &now, row.LastErrorAt)
			require.EqualValues(t, 1, row.ConsecutiveFailures)
			require.Equal(t, &period, row.ExpectedPeriodSeconds)
			return
		}
	}
	t.Fatal("worker state missing after restart")
}
