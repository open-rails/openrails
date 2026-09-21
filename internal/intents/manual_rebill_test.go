package intents

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestManualRebillIdentityPreservesPeriodPrecisionAndAttempt(t *testing.T) {
	sub := uuid.New()
	period := time.Date(2026, 6, 1, 0, 0, 0, 123456000, time.UTC)
	first := subscriptions.ManualRebillIdempotencyKey(sub, period, "nmi", 0)
	require.Equal(t, first, subscriptions.ManualRebillIdempotencyKey(sub, period, "NMI", 0))
	require.NotEqual(t, first, subscriptions.ManualRebillIdempotencyKey(sub, period.Add(time.Microsecond), "nmi", 0))
	second := subscriptions.ManualRebillIdempotencyKey(sub, period, "nmi", 1)
	require.NotEqual(t, first, second)
	require.NotEqual(t, subscriptions.RebillOrderReference(first), subscriptions.RebillOrderReference(second), "another attempt cannot present this attempt's receipt")
}
