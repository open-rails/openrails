package alerting

import (
	"encoding/json"
	"testing"
	"time"
)

func TestGenericWebhookBodyStatesFiredAtInUTC(t *testing.T) {
	// opened_at as pgx reads it: the session's zone, not UTC.
	fired := time.Date(2026, 11, 23, 9, 30, 0, 123456000, time.FixedZone("UTC+2", 2*60*60))
	body, err := shapeWebhookBody(FormatGeneric, Alert{Title: "held", Severity: SeverityCritical, Summary: "held", FiredAt: fired})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		FiredAt string `json:"fired_at"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.FiredAt != "2026-11-23T07:30:00.123456Z" {
		t.Fatalf("fired_at %q, want the same instant in UTC", got.FiredAt)
	}
}
