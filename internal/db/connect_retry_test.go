package db

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// xs-007 row 40: the connect wait has no budget of its own. A database that
// answers late is connected to; only the caller's context ends the wait.
func TestPingWithRetry_WaitsForTheDatabaseNotAClock(t *testing.T) {
	var calls atomic.Int32
	ping := func(context.Context) error {
		if calls.Add(1) < 3 {
			return errors.New("connection refused")
		}
		return nil
	}
	start := time.Now()
	require.NoError(t, pingWithRetry(context.Background(), ping, "test db"))
	require.EqualValues(t, 3, calls.Load())
	require.GreaterOrEqual(t, time.Since(start), dbConnectBaseDelay, "backed off between attempts")
}

func TestPingWithRetry_OnlyTheCallerEndsTheWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel() // the operator's stop signal
	}()
	err := pingWithRetry(ctx, func(context.Context) error { return errors.New("still failing over") }, "test db")
	require.ErrorIs(t, err, context.Canceled)
	require.Contains(t, err.Error(), "still failing over", "the last observed failure travels with the reason")
}
