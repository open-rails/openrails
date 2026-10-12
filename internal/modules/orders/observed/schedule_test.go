package observed

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestAcceptedProviderCycleIdentity(t *testing.T) {
	first := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		hours, months int
		next, at      string
		approved      bool
		want          string
		bad           bool
	}{
		{"fixed days", 720, 0, "2026-04-01", "2026-03-03", true, "2026-03-02", false},
		{"calendar February", 0, 1, "2026-02-28", "2026-02-02", true, "2026-01-31", false},
		{"calendar March retains day", 0, 1, "2026-03-31", "2026-03-01", true, "2026-02-28", false},
		{"decline next unpaid cycle", 0, 1, "2026-02-28", "2026-03-01", false, "2026-02-28", false},
		{"changed retry date", 0, 1, "2026-03-02", "2026-03-01", true, "", true},
		{"receipt outside cycle", 0, 1, "2026-03-31", "2026-04-01", true, "", true},
		{"before first cycle", 0, 1, "2026-01-31", "2026-01-30", true, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := time.Parse("2006-01-02", tc.next)
			require.NoError(t, err)
			at, err := time.Parse("2006-01-02", tc.at)
			require.NoError(t, err)
			due, err := scheduleReceiptCycle(first, tc.hours, tc.months, ScheduleReceipt{NextBillingAt: next, OccurredAt: at, Approved: tc.approved})
			if tc.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, due.Format("2006-01-02"))
			require.Equal(t, 12, due.Hour())
		})
	}
}
