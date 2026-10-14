package observed

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
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

func TestApprovedReceiptsSettleTheOldestExecutedCycle(t *testing.T) {
	first := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	schedule := gen.BillingProviderSchedule{FirstDueAt: first, CadenceMonths: 1, AcceptedBill: []byte(`{}`)}
	day := func(s string) time.Time {
		v, err := time.Parse("2006-01-02", s)
		require.NoError(t, err)
		return v
	}
	for _, tc := range []struct {
		name     string
		unpaid   int
		next, at string
		want     string
	}{
		{"on time", 1, "2026-03-31", "2026-02-28", "2026-02-28"},
		{"catch-up charge settles the older cycle", 0, "2026-03-31", "2026-03-01", "2026-01-31"},
		{"extra capture of a settled cycle", 2, "2026-03-31", "2026-03-01", "2026-02-28"},
		{"before the unpaid cycle is due", 1, "2026-03-31", "2026-02-27", ""},
		{"at or after the provider's next boundary", 0, "2026-03-31", "2026-03-31", ""},
		{"no executed cycle", 0, "2026-01-31", "2026-01-31", ""},
		{"changed retry date", 0, "2026-03-02", "2026-03-01", ""},
		{"an off-cadence date is no boundary", 1, "2076-03-02", "2026-03-01", "2026-02-28"},
		{"ended schedule inside the unpaid cycle", 1, "", "2026-03-01", "2026-02-28"},
		{"ended schedule after the unpaid cycle", 0, "", "2026-03-01", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var next time.Time
			if tc.next != "" {
				next = day(tc.next)
			}
			due, err := approvedReceiptCycle(schedule, tc.unpaid, ScheduleReceipt{NextBillingAt: next, OccurredAt: day(tc.at).Add(15 * time.Hour), Approved: true})
			if tc.want == "" {
				require.ErrorIs(t, err, errNoCycle)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, due.Format("2006-01-02"))
		})
	}
}

func TestCalendarAmendmentRetainsOriginalDayAfterFebruaryClampsItsFirstDue(t *testing.T) {
	anchor := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	first := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	next := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(Bill{CalendarAnchor: &anchor})
	require.NoError(t, err)
	amendment := gen.BillingProviderSchedule{FirstDueAt: first, CadenceMonths: 1, AcceptedBill: raw}
	frozen, err := CalendarScheduleAnchor(amendment, next)
	require.NoError(t, err)
	require.Equal(t, &anchor, frozen)
	receipt := ScheduleReceipt{TransactionID: "new-plan-payment", OccurredAt: first.Add(time.Hour), NextBillingAt: next, Approved: true}
	due, err := approvedReceiptCycle(amendment, 0, receipt)
	require.NoError(t, err)
	require.Equal(t, first, due)
	// Reconstruct the immutable acceptance as archive/restart does.
	var restored Bill
	require.NoError(t, json.Unmarshal(raw, &restored))
	amendment.AcceptedBill, err = json.Marshal(restored)
	require.NoError(t, err)
	replayed, err := approvedReceiptCycle(amendment, 0, receipt)
	require.NoError(t, err)
	require.Equal(t, due, replayed)
	wrong := next.AddDate(0, 0, -3)
	_, err = CalendarScheduleAnchor(amendment, wrong)
	require.Error(t, err, "March28 is not a boundary of the accepted January31 agreement")
	_, err = declinedReceiptCycle(amendment, 0, ScheduleReceipt{DueAt: anchor, OccurredAt: first})
	require.ErrorIs(t, err, errNoCycle, "new terms cannot claim a cycle before their first effective boundary")
}
