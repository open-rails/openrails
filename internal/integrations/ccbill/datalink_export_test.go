package ccbill

import (
	"testing"
	"time"
)

func TestDataLinkWindowsAreADayAtMostAndContiguous(t *testing.T) {
	start := time.Date(2026, 11, 1, 6, 30, 0, 500_000_000, time.UTC)
	end := start.Add(50 * time.Hour)
	got := dataLinkWindows(start, end)
	want := [][2]time.Time{
		{start.Truncate(time.Second), start.Truncate(time.Second).Add(24*time.Hour - time.Second)},
		{start.Truncate(time.Second).Add(24 * time.Hour), start.Truncate(time.Second).Add(48*time.Hour - time.Second)},
		{start.Truncate(time.Second).Add(48 * time.Hour), end.Truncate(time.Second).Add(-time.Second)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d windows, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i][0].Equal(want[i][0]) || !got[i][1].Equal(want[i][1]) {
			t.Errorf("window %d = %v, want %v", i, got[i], want[i])
		}
	}
	if w := dataLinkWindows(start, start.Add(time.Minute)); len(w) != 1 || w[0][1].Sub(w[0][0]) != time.Minute-time.Second {
		t.Errorf("a minute: %v", w)
	}
	if w := dataLinkWindows(start, start); len(w) != 0 {
		t.Errorf("an empty range asks nothing: %v", w)
	}
}

func TestDataLinkExportRowsReadTheGuidesDefaultColumns(t *testing.T) {
	signup := DataLinkExportRow{TransactionType: DataLinkTxnNew, Fields: []string{"NEW", "999999", "0000", "0123", "20240105103000", "Ann", "Lee", "ann", "pw",
		"1 Main", "Town", "AZ", "85001", "US", "ann@example.com", "", "ACTIVE", "2.95", "2", "23.00", "30", "Y", "VISA", "", ""}}
	got, ok := signup.Signup()
	want := Signup{Username: "ann", Email: "ann@example.com", InitialAmount: "2.95", InitialDays: 2, RecurringAmount: "23.00", RecurringDays: 30}
	if !ok || got != want || signup.SubscriptionID() != "0123" || signup.Timestamp() != "20240105103000" {
		t.Fatalf("NEW row read as %+v (%v)", got, ok)
	}
	expire := DataLinkExportRow{TransactionType: DataLinkTxnExpire, Fields: []string{"EXPIRE", "999999", "0000", "0123", "2024-04-05", "2024-03-20", "N"}}
	if expire.ExpireDate() != "2024-04-05" || expire.CancelDate() != "2024-03-20" {
		t.Fatalf("EXPIRE row read expire %q cancel %q", expire.ExpireDate(), expire.CancelDate())
	}
	if _, ok := expire.Signup(); ok {
		t.Fatal("an EXPIRE row is no signup")
	}
}
