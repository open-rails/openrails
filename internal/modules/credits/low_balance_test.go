package credits

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

// intPtr / int64Ptr keep the resolveDepositExpiry table compact.
func intPtr(v int) *int       { return &v }
func i64Ptr(v int64) *int64   { return &v }
func tPtr(t time.Time) *time.Time { return &t }

func TestResolveDepositExpiry_Precedence(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	explicit := now.Add(48 * time.Hour)

	cases := []struct {
		name          string
		globalDays    int
		ctDefaultDays *int
		explicit      *time.Time
		wantNil       bool
		wantExpiry    time.Time
	}{
		{
			name:       "explicit wins over everything",
			globalDays: 365,
			ctDefaultDays: intPtr(10),
			explicit:   tPtr(explicit),
			wantExpiry: explicit,
		},
		{
			name:          "per-type default applied when no explicit",
			globalDays:    365,
			ctDefaultDays: intPtr(10),
			wantExpiry:    now.Add(10 * 24 * time.Hour),
		},
		{
			name:       "global 365-day default applied when no explicit and no per-type",
			globalDays: 365,
			wantExpiry: now.Add(365 * 24 * time.Hour),
		},
		{
			name:    "no expiry at all stays non-expiring",
			wantNil: true,
		},
		{
			name:          "zero per-type default falls back to global",
			globalDays:    365,
			ctDefaultDays: intPtr(0),
			wantExpiry:    now.Add(365 * 24 * time.Hour),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &CreditsService{}
			s.SetDefaultExpiryDays(tc.globalDays)
			ct := &models.CreditType{DefaultCreditExpiryDays: tc.ctDefaultDays}
			got := s.resolveDepositExpiry(now, ct, tc.explicit)
			if tc.wantNil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, tc.wantExpiry.UTC(), got.UTC())
		})
	}
}

func TestMultiLowBalanceSink_FansOutAndCollectsFirstError(t *testing.T) {
	var seen []string
	boom := errors.New("boom")

	a := LowBalanceSinkFunc(func(_ context.Context, ev LowBalanceEvent) error {
		seen = append(seen, "a")
		return boom
	})
	b := LowBalanceSinkFunc(func(_ context.Context, ev LowBalanceEvent) error {
		seen = append(seen, "b")
		return nil
	})

	multi := MultiLowBalanceSink{a, nil, b}
	err := multi.Handle(context.Background(), LowBalanceEvent{})
	require.ErrorIs(t, err, boom)
	// Every non-nil sink runs even though an earlier one errored.
	require.Equal(t, []string{"a", "b"}, seen)
}
