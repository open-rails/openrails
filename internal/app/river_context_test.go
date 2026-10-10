package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// River's notifier spins on a context ended by deadline (v0.47); River's
// context ends only by cancellation, when the caller's ends.
func TestRiverSeesOnlyCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	run := cancelOnly(ctx)
	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("River's context outlived the caller's")
	}
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.ErrorIs(t, run.Err(), context.Canceled)
	_, hasDeadline := run.Deadline()
	require.False(t, hasDeadline)
}
