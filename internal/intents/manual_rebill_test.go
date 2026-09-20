package intents

import (
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestManualRebillIdentityPreservesPeriodPrecisionAndAttempt(t *testing.T) {
	sub := uuid.New()
	period := time.Date(2026, 6, 1, 0, 0, 0, 123456000, time.UTC)
	first := ManualRebillIdempotencyKey(sub, period, "nmi", 0)
	require.Equal(t, first, ManualRebillIdempotencyKey(sub, period, "NMI", 0))
	require.NotEqual(t, first, ManualRebillIdempotencyKey(sub, period.Add(time.Microsecond), "nmi", 0))
	second := ManualRebillIdempotencyKey(sub, period, "nmi", 1)
	require.NotEqual(t, first, second)
	require.NotEqual(t, rebillOrderReference(first), rebillOrderReference(second), "another attempt cannot present this attempt's receipt")
}
