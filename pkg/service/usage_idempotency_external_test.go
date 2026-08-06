package service_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/service"
)

func TestNewUsageIdempotencyKey(t *testing.T) {
	t.Parallel()

	key, err := service.NewUsageIdempotencyKey("payment.succeeded", "host-settlement", "pay-123")
	require.NoError(t, err)
	_ = service.RecordUsageInput{Key: key}

	_, err = service.NewUsageIdempotencyKey("payment.succeeded", "", "pay-123")
	require.Error(t, err)
}
