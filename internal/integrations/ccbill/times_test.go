package ccbill

import (
	"testing"
	"time"
)

func TestCCBillTimesReadInOneZone(t *testing.T) {
	at := time.Date(2026, 11, 20, 3, 22, 44, 0, time.UTC)
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	for raw, want := range map[string]time.Time{
		"2026-11-20 03:22:44":  at, // a webhook timestamp
		"20261120032244":       at, // a DataLink or status timestamp
		"2026-11-20T03:22:44Z": at,
		"2026-11-20":           day, // a REBILL or EXPIRE timestamp, a rebill or expire date
		"20261120":             day, // a status date
		"11/20/2026":           day,
		" 2026-11-20 ":         day,
	} {
		got, err := ParseTime(raw)
		if err != nil || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("ParseTime(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := ParseInstant("2026-11-20"); err == nil {
		t.Error("a date read as an instant")
	}
	if _, err := ParseDate("2026-11-20 03:22:44"); err == nil {
		t.Error("an instant read as a date")
	}
	for _, raw := range []string{"", "yesterday", "2026-13-01"} {
		if _, err := ParseTime(raw); err == nil {
			t.Errorf("ParseTime(%q) read", raw)
		}
	}
	if got := DayEnd(at); !got.Equal(day.Add(24*time.Hour - time.Second)) {
		t.Errorf("DayEnd = %v", got)
	}
}

func TestSubscriptionStatusExpiry(t *testing.T) {
	for raw, want := range map[string]time.Time{
		"20261120":       time.Date(2026, 11, 20, 23, 59, 59, 0, time.UTC),
		"20261120000000": time.Date(2026, 11, 20, 23, 59, 59, 0, time.UTC),
		"20261120120000": time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	} {
		got, ok := SubscriptionStatusResult{Fields: map[string]string{subscriptionStatusExpiryFields[0]: raw}}.ExpiresAt()
		if !ok || !got.Equal(want) {
			t.Errorf("ExpiresAt(%q) = %v, %v; want %v", raw, got, ok, want)
		}
	}
	if _, ok := (SubscriptionStatusResult{Fields: map[string]string{}}).ExpiresAt(); ok {
		t.Error("no date read as one")
	}
}
