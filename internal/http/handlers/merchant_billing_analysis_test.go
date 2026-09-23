package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/billinganalysis"
)

func TestBillingAnalysisCalendarDaysUsesLocalDates(t *testing.T) {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	to := time.Date(2026, 3, 9, 23, 59, 59, 0, loc)
	if got := billingAnalysisCalendarDays(from, to, loc); got != 2 {
		t.Fatalf("calendar days = %d, want 2", got)
	}
}

func TestToBillingAnalysisResponsePreservesDailyEvidence(t *testing.T) {
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	raw := json.RawMessage(`{"source":"provider"}`)
	report := &billinganalysis.Report{
		From: "2026-09-23T00:00:00Z", To: "2026-09-23T23:59:59Z", Timezone: "UTC",
		Days:     []billinganalysis.Day{{Date: "2026-09-23", SettledSignups: 1, FailedRebills: 1}},
		Charges:  []billinganalysis.Charge{{Day: "2026-09-23", Kind: "signup", Provider: "nmi", EventKey: "charge-1", CustomerRef: "vault-1", AmountCents: 1200, Currency: "USD", OccurredAt: at, Raw: raw}},
		Failures: []billinganalysis.Failure{{Charge: billinganalysis.Charge{Day: "2026-09-23", Kind: "rebill", Provider: "nmi", EventKey: "failure-1", CustomerRef: "vault-2", AmountCents: 1200, Currency: "USD", OccurredAt: at, Raw: raw}, DeclineCode: "declined", FailureCount: 2}},
	}
	response := toBillingAnalysisResponse(report)
	if len(response.Daily) != 1 || len(response.Daily[0].Charges) != 1 || len(response.Daily[0].Failures) != 1 {
		t.Fatalf("daily evidence was dropped: %#v", response.Daily)
	}
	if response.Daily[0].Charges[0].CustomerRef != "vault-1" || response.Daily[0].Failures[0].DeclineCode != "declined" {
		t.Fatalf("provider evidence was not preserved: %#v", response.Daily[0])
	}
	if string(response.Daily[0].Charges[0].Raw) != string(raw) {
		t.Fatalf("raw payload changed: %s", response.Daily[0].Charges[0].Raw)
	}
}
