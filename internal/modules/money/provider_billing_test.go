package money

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrepareProviderBillingObservationRefusalsAndOverflow(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	base := ProviderBillingObservationInput{
		OperationID:     "op-1",
		ObservationID:   "obs-1",
		NormalizedQuery: "podId=provider-resource-1",
		QueryStart:      now.Add(-time.Hour),
		QueryEnd:        now,
		RawBody:         []byte(`[]`),
		Lifecycle: ProviderBillingLifecycleEvidence{
			Provider: "provider", ProviderResourceID: "provider-resource-1",
			ProviderLifetimeStart: now.Add(-time.Hour), ProviderLifetimeEnd: now.Add(-time.Minute),
			ProviderAbsentAt: now, ProviderAbsenceReference: "absence:1",
			BillingStopReference: "billing-stop:1", WindowsClosedAt: now,
			WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{"absent":true}`),
		},
	}

	for _, kind := range []string{"schema_ambiguity", "submicro_amount", "amount_overflow", "response_too_large"} {
		in := base
		in.RawBody = nil
		in.Refusal = &ProviderBillingObservationRefusal{Kind: ProviderBillingEvidenceRefusalKind(kind)}
		require.NoError(t, validateProviderBillingInput(in))
		prepared, err := prepareProviderBillingObservation(in)
		require.NoError(t, err)
		require.Equal(t, kind, *prepared.refusalKind)
		require.False(t, prepared.rawAvailable)
	}

	overflow := base
	overflow.Records = []ProviderBillingRecord{
		{ProviderResourceID: base.Lifecycle.ProviderResourceID, BucketStart: now.Add(-time.Hour), AmountUSDMicros: math.MaxInt64, TimeBilledMS: 1},
		{ProviderResourceID: base.Lifecycle.ProviderResourceID, BucketStart: now.Add(-time.Minute), AmountUSDMicros: 1, TimeBilledMS: 1},
	}
	_, err := prepareProviderBillingObservation(overflow)
	require.ErrorContains(t, err, "total USD micros overflow")

	negative := base
	negative.Records = []ProviderBillingRecord{{
		ProviderResourceID: base.Lifecycle.ProviderResourceID,
		BucketStart:        now.Add(-time.Hour), AmountUSDMicros: -1, TimeBilledMS: 1,
	}}
	prepared, err := prepareProviderBillingObservation(negative)
	require.NoError(t, err)
	require.True(t, prepared.hasNegative)
}
