package models

import (
	"encoding/json"
	"testing"
)

func TestCreditsSpec_UnmarshalJSON_V2(t *testing.T) {
	var cs CreditsSpec
	raw := []byte(`{
		"api_credits": {"amount": 1000, "unit": "USD", "expires_days": 30, "cadence": "once"},
		"gpu_minutes": {"amount": 6000, "unit": "USD", "expires_days": 7, "cadence": "per_renewal"},
		"eur_promo":   {"amount": 500, "unit": "EUR", "expiry_days": 90}
	}`)
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cs) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(cs))
	}
	if cs["api_credits"].Amount != 1000 {
		t.Fatalf("unexpected api_credits amount: %d", cs["api_credits"].Amount)
	}
	if cs["gpu_minutes"].Cadence != CreditGrantCadencePerRenewal {
		t.Fatalf("unexpected gpu_minutes cadence: %s", cs["gpu_minutes"].Cadence)
	}
	// new fields: unit + expiry_days parse.
	if got := cs["eur_promo"].UnitCode(); got != "EUR" {
		t.Fatalf("eur_promo unit = %q, want EUR", got)
	}
	if got := cs["eur_promo"].EffectiveExpiryDays(); got != 90 {
		t.Fatalf("eur_promo expiry = %d, want 90", got)
	}
	if got := cs["api_credits"].UnitCode(); got != "USD" {
		t.Fatalf("api_credits unit = %q, want USD", got)
	}
}

func TestCreditGrantSpec_UnitAndExpiryDefaults(t *testing.T) {
	// unit has no default; explicit unit preserved.
	if got := (CreditGrantSpec{}).UnitCode(); got != "" {
		t.Fatalf("blank unit = %q, want empty", got)
	}
	if got := (CreditGrantSpec{Unit: "EUR"}).UnitCode(); got != "EUR" {
		t.Fatalf("explicit unit = %q, want EUR", got)
	}
	// expiry: omitted => 365; explicit 0 => never (0); legacy expires_days honored.
	if got := (CreditGrantSpec{}).EffectiveExpiryDays(); got != DefaultCreditGrantExpiryDays {
		t.Fatalf("default expiry = %d, want %d", got, DefaultCreditGrantExpiryDays)
	}
	zero := 0
	if got := (CreditGrantSpec{ExpiryDays: &zero}).EffectiveExpiryDays(); got != 0 {
		t.Fatalf("explicit 0 expiry = %d, want 0 (never)", got)
	}
	legacy := 30
	if got := (CreditGrantSpec{ExpiresDays: &legacy}).EffectiveExpiryDays(); got != 30 {
		t.Fatalf("legacy expires_days = %d, want 30", got)
	}
	// expiry_days takes precedence over legacy expires_days.
	newer := 7
	if got := (CreditGrantSpec{ExpiryDays: &newer, ExpiresDays: &legacy}).EffectiveExpiryDays(); got != 7 {
		t.Fatalf("precedence expiry = %d, want 7", got)
	}
}

func TestPrice_GetCCBillFlexForm_RequiresFlexID(t *testing.T) {
	price := &Price{Rails: map[string]map[string]string{
		string(RailCCBill): {
			RailKeyCCBillFormName: "form-name",
			RailKeyStripePriceID:  "stripe-price-id",
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
