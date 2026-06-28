package db

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPingWithRetryHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pingWithRetry(ctx, func(context.Context) error {
		return errors.New("not ready")
	}, "database")

	require.ErrorIs(t, err, context.Canceled)
}
