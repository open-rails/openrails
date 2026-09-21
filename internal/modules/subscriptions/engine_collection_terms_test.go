package subscriptions

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestEngineRecoveryPeriodSelection(t *testing.T) {
	oldEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accepted := RenewalTerms{PSPID: uuid.New(), SubscriptionID: uuid.New(), CustomerID: uuid.New(), FromPriceID: uuid.New(), FromProductID: uuid.New(), PriceID: uuid.New(), ProductID: uuid.New(), Amount: 10_000_000, Currency: "USD", PeriodStart: oldEnd, PeriodEnd: oldEnd.Add(30 * 24 * time.Hour)}
	for _, tc := range []struct {
		name      string
		now, want time.Time
	}{
		{"on time", oldEnd, oldEnd},
		{"within next period", oldEnd.Add(29 * 24 * time.Hour), oldEnd},
		{"whole period missed", accepted.PeriodEnd, accepted.PeriodEnd},
		{"several missed periods", oldEnd.Add(100 * 24 * time.Hour), oldEnd.Add(100 * 24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terms, err := SelectEngineRenewalPeriod(accepted, tc.now)
			require.NoError(t, err)
			require.Equal(t, tc.want, terms.PeriodStart)
			require.Equal(t, tc.want.Add(30*24*time.Hour), terms.PeriodEnd)
			require.Equal(t, accepted.Amount, terms.Amount)
		})
	}
	_, err := SelectEngineRenewalPeriod(accepted, oldEnd.Add(-time.Second))
	require.Error(t, err)
	// A definitively refused attempt grants nothing. A separately admitted later
	// attempt can select another start against the unchanged old-period boundary.
	first, err := SelectEngineRenewalPeriod(accepted, oldEnd.Add(100*24*time.Hour))
	require.NoError(t, err)
	second, err := SelectEngineRenewalPeriod(accepted, oldEnd.Add(101*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, first.PeriodStart.Add(24*time.Hour), second.PeriodStart)
	require.Equal(t, oldEnd, accepted.PeriodStart)
}
