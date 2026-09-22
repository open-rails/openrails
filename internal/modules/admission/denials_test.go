package admission

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestDenialRecorderWithoutRedis(t *testing.T) {
	var client *redis.Client
	recorder := NewDenialRecorder(client)
	require.NotPanics(t, func() {
		recorder.Record(context.Background(), "merchant", "customer", "budget_exceeded", time.Now())
	})
}
