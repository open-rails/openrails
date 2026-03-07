package models

import (
	"encoding/json"
	"testing"
)

func TestCreditsSpec_UnmarshalJSON_V2(t *testing.T) {
	var cs CreditsSpec
	raw := []byte(`{
		"api_credits": {"amount": 1000, "expires_days": 30, "cadence": "once"},
		"gpu_minutes": {"amount": 6000, "expires_days": 7, "cadence": "per_renewal"}
	}`)
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(cs))
	}
	if cs["api_credits"].Amount != 1000 {
		t.Fatalf("unexpected api_credits amount: %d", cs["api_credits"].Amount)
	}
	if cs["gpu_minutes"].Cadence != CreditGrantCadencePerRenewal {
		t.Fatalf("unexpected gpu_minutes cadence: %s", cs["gpu_minutes"].Cadence)
	}
}

func TestCreditsSpec_UnmarshalJSON_InvalidLegacyShape(t *testing.T) {
	var cs CreditsSpec
	raw := []byte(`{"promo_amount_cents": 499, "promo_expires_days": 10, "grant_on": "initial"}`)
	if err := json.Unmarshal(raw, &cs); err == nil {
		t.Fatal("expected legacy credits_spec shape to fail")
	}
}
