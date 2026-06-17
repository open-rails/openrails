package audit

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTableFormatterSummaryOmitsFindingDetails(t *testing.T) {
	finding := Finding{
		CheckID:        "P-E-1",
		CheckName:      "Completed payment missing entitlements",
		Severity:       SeverityHigh,
		EntityType:     EntityPayment,
		EntityID:       uuid.MustParse("5b830eda-baca-5dd9-8921-9094b718d5db"),
		UserID:         "user-1",
		Description:    "Completed one-off payment has no entitlements granted",
		Recommendation: "Grant entitlements",
		AutoFixable:    true,
	}
	summary := Summary{
		TotalFindings:    1,
		BySeverity:       map[Severity]int{SeverityHigh: 1},
		ByCategory:       map[string]int{"payment_entitlement": 1},
		AutoFixable:      1,
		ManualReviewOnly: 0,
	}

	var out bytes.Buffer
	if err := (&TableFormatter{}).FormatSummary(&out, []Finding{finding}, summary, "audit.log"); err != nil {
		t.Fatalf("format summary: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Total findings:     1",
		"Detailed log:       audit.log",
		"By Check:",
		"P-E-1",
		"Completed payment missing entitlements",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"Entity:",
		"user-1",
		"Completed one-off payment has no entitlements granted",
		"Duration:",
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("summary should not contain %q:\n%s", unwanted, got)
		}
	}
}

func TestTableFormatterDetailsIncludesFindingDetails(t *testing.T) {
	finding := Finding{
		CheckID:        "SS-1",
		CheckName:      "Active subscription past period end",
		Severity:       SeverityHigh,
		EntityType:     EntitySubscription,
		EntityID:       uuid.MustParse("bf3a91b6-1740-56dd-bbc7-bbd6abace572"),
		UserID:         "user-2",
		Description:    "Subscription is active but period ended",
		Recommendation: "Transition to past_due",
		AutoFixable:    true,
	}
	summary := Summary{
		TotalFindings:    1,
		BySeverity:       map[Severity]int{SeverityHigh: 1},
		ByCategory:       map[string]int{"subscription_state": 1},
		AutoFixable:      1,
		ManualReviewOnly: 0,
	}

	var out bytes.Buffer
	if err := (&TableFormatter{}).FormatDetails(&out, []Finding{finding}, summary); err != nil {
		t.Fatalf("format details: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Check IDs are internal OpenRails audit labels",
		"[HIGH] SS-1",
		"Entity: subscription bf3a91b6-1740-56dd-bbc7-bbd6abace572",
		"User:   user-2",
		"Subscription is active but period ended",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("details missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Duration:") {
		t.Fatalf("details should not contain duration:\n%s", got)
	}
}
