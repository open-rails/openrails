package money

import (
	"testing"
	"time"
)

// Consecutive invoice periods must tile time: no day is skipped or billed twice,
// including anchors on days some months lack.
func TestInvoicePeriodsAreContiguous(t *testing.T) {
	anchors := []time.Time{
		time.Date(2026, 1, 31, 9, 30, 0, 0, time.UTC),
		time.Date(2026, 1, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
	}
	for _, boundary := range []string{InvoiceBoundaryAnniversary, InvoiceBoundaryCalendarMonth, InvoiceBoundaryFixedInterval} {
		for _, anchor := range anchors {
			for now := anchor.AddDate(0, 1, 0); now.Before(anchor.AddDate(2, 0, 0)); now = now.Add(13 * time.Hour) {
				from, to, err := PreviousInvoicePeriod(now, anchor, boundary)
				if err != nil {
					t.Fatal(err)
				}
				if to.After(now) || !from.Before(to) {
					t.Fatalf("%s anchor %s now %s: bad period [%s, %s)", boundary, anchor, now, from, to)
				}
				prev, err := CurrentInvoicePeriodStart(to.Add(-time.Nanosecond), anchor, boundary)
				if err != nil || !prev.Equal(from) {
					t.Fatalf("%s anchor %s now %s: period [%s, %s) does not follow the one starting %s", boundary, anchor, now, from, to, prev)
				}
			}
		}
	}
	got, _ := CurrentInvoicePeriodStart(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC), anchors[0], InvoiceBoundaryAnniversary)
	if want := time.Date(2026, 3, 31, 9, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("anniversary start = %s, want %s", got, want)
	}
}
