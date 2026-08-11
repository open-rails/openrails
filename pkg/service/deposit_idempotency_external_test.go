package service_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/service"
)

func TestNewDepositIdempotencyKey(t *testing.T) {
	t.Parallel()

	key, err := service.NewDepositIdempotencyKey("admin", "grant-2026-08-batch-7")
	require.NoError(t, err)
	_ = service.DepositCreditsRequest{Key: key}

	_, err = service.NewDepositIdempotencyKey("admin", "")
	require.Error(t, err)
	_, err = service.NewDepositIdempotencyKey("", "grant-2026-08-batch-7")
	require.Error(t, err)
}
