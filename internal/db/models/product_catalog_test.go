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

func TestPrice_GetCCBillFlexForm_RequiresFlexID(t *testing.T) {
	price := &Price{Processors: map[string]map[string]string{
		string(ProcessorCCBill): {
			ProcessorKeyCCBillFormName: "form-name",
			ProcessorKeyStripePriceID:  "stripe-price-id",
		},
	}}

	_, _, ok := price.GetCCBillFlexForm()
	if ok {
		t.Fatal("expected CCBill config without flex_id to be rejected")
	}
}

func TestCatalogStatus_Valid(t *testing.T) {
	for _, s := range []CatalogStatus{CatalogStatusDraft, CatalogStatusActive, CatalogStatusArchived} {
		if !s.Valid() {
			t.Fatalf("expected %q to be valid", s)
		}
	}
	for _, s := range []CatalogStatus{"", "live", "deleted", "ACTIVE"} {
		if s.Valid() {
			t.Fatalf("expected %q to be invalid", s)
		}
	}
}

func TestCatalogStatus_PurchasableAndBillable(t *testing.T) {
	cases := []struct {
		status          CatalogStatus
		wantPurchasable bool
		wantBillable    bool
	}{
		{CatalogStatusDraft, false, false},
		{CatalogStatusActive, true, true},
		// archived: not purchasable (new buyers blocked) but still billable
		// (existing subscriptions are grandfathered and bill forever).
		{CatalogStatusArchived, false, true},
	}
	for _, c := range cases {
		prod := &Product{Status: c.status}
		if got := prod.IsPurchasable(); got != c.wantPurchasable {
			t.Fatalf("Product(%q).IsPurchasable()=%v want %v", c.status, got, c.wantPurchasable)
		}
		if got := prod.IsBillable(); got != c.wantBillable {
			t.Fatalf("Product(%q).IsBillable()=%v want %v", c.status, got, c.wantBillable)
		}
		price := &Price{Status: c.status}
		if got := price.IsPurchasable(); got != c.wantPurchasable {
			t.Fatalf("Price(%q).IsPurchasable()=%v want %v", c.status, got, c.wantPurchasable)
		}
		if got := price.IsBillable(); got != c.wantBillable {
			t.Fatalf("Price(%q).IsBillable()=%v want %v", c.status, got, c.wantBillable)
		}
	}
}
