package grants

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// A derived window is [start, end) with end strictly after start, preferring the current period.
func TestSubscriptionWindow(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	periodEnd, ended := start.Add(720*time.Hour), start.Add(100*time.Hour)
	ptr := func(x time.Time) *time.Time { return &x }
	for name, tc := range map[string]struct {
		row        gen.ListUngrantedSubscriptionsRow
		start, end time.Time
	}{
		"period bounds preferred":        {gen.ListUngrantedSubscriptionsRow{StartedAt: start, CurrentPeriodStartsAt: ptr(start), CurrentPeriodEndsAt: ptr(periodEnd), EndedAt: ptr(ended)}, start, periodEnd},
		"falls back to started..ended":   {gen.ListUngrantedSubscriptionsRow{StartedAt: start, EndedAt: ptr(ended)}, start, ended},
		"no end is not a window":         {row: gen.ListUngrantedSubscriptionsRow{StartedAt: start}},
		"end not after start is refused": {row: gen.ListUngrantedSubscriptionsRow{StartedAt: start, CurrentPeriodStartsAt: ptr(periodEnd), CurrentPeriodEndsAt: ptr(start)}},
	} {
		s, e, ok := subscriptionWindow(tc.row)
		require.Equal(t, !tc.end.IsZero(), ok, name)
		require.True(t, s.Equal(tc.start) && e.Equal(tc.end), "%s: [%s,%s)", name, s, e)
	}
}

func TestProductSpecKeys(t *testing.T) {
	for raw, want := range map[string][]string{
		`{"vip": null, "premium": 720}`: {"premium", "vip"},
		`{}`:                            {},
		`{"  premium  ": 1, "": 2}`:     {"premium"},
		`not-json`:                      nil,
		``:                              nil,
	} {
		require.Equal(t, want, productSpecKeys([]byte(raw)), raw)
	}
}

// An empty or inverted wallet window is a no-op that never reaches the database (nil queries).
func TestDeriveWalletGrantSkipsEmptyWindow(t *testing.T) {
	l := &Ledger{merchant: uuid.New()}
	now := time.Now().UTC()
	for _, exp := range []time.Time{now, now.Add(-time.Hour)} {
		require.NoError(t, l.DeriveWalletGrant(context.Background(), gen.ListUngrantedWalletPaymentsRow{ID: uuid.New(), CustomerID: uuid.New(), PurchasedAt: now, ExpiresAt: exp}))
	}
}
