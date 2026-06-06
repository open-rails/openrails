package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPostgresConnectorRetrySettingsAreHardcoded(t *testing.T) {
	require.Equal(t, 60*time.Second, dbConnectMaxWait)
	require.Equal(t, time.Second, dbConnectBaseDelay)
	require.Equal(t, 5*time.Second, dbConnectMaxDelay)
	require.Equal(t, 5*time.Second, dbConnectPingTimeout)
}

func TestPingWithRetryHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pingWithRetry(ctx, func(context.Context) error {
		return errors.New("not ready")
	}, "database")

	require.ErrorIs(t, err, context.Canceled)
}
