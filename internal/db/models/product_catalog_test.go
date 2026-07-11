package models

import (
	"encoding/json"
	"testing"
)

func TestCreditsSpec_UnmarshalJSON_V2(t *testing.T) {
	var cs CreditsSpec
	raw := []byte(`{
		"api_credits": {"amount": 1000, "unit": "USD", "expiry_hours": 30, "cadence": "once"},
		"gpu_minutes": {"amount": 6000, "unit": "USD", "expiry_hours": 7, "cadence": "per_renewal"},
		"eur_promo":   {"amount": 500, "unit": "EUR", "expiry_hours": 90}
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
	// new fields: unit + expiry_hours parse.
	if got := cs["eur_promo"].UnitCode(); got != "EUR" {
		t.Fatalf("eur_promo unit = %q, want EUR", got)
	}
	if got := cs["eur_promo"].EffectiveExpiryHours(); got != 90 {
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
	// expiry: omitted => default; explicit 0 => never.
	if got := (CreditGrantSpec{}).EffectiveExpiryHours(); got != DefaultCreditGrantExpiryHours {
		t.Fatalf("default expiry = %d, want %d", got, DefaultCreditGrantExpiryHours)
	}
	zero := 0
	if got := (CreditGrantSpec{ExpiryHours: &zero}).EffectiveExpiryHours(); got != 0 {
		t.Fatalf("explicit 0 expiry = %d, want 0 (never)", got)
	}
	explicit := 7
	if got := (CreditGrantSpec{ExpiryHours: &explicit}).EffectiveExpiryHours(); got != 7 {
		t.Fatalf("explicit expiry = %d, want 7", got)
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

func TestArchived_Purchasable(t *testing.T) {
	for _, archived := range []bool{false, true} {
		want := !archived
		if got := (&Product{Archived: archived}).IsPurchasable(); got != want {
			t.Fatalf("Product(archived=%v).IsPurchasable()=%v want %v", archived, got, want)
		}
		if got := (&Price{Archived: archived}).IsPurchasable(); got != want {
			t.Fatalf("Price(archived=%v).IsPurchasable()=%v want %v", archived, got, want)
		}
	}
}

func TestRailLinkEntries_AccountKeyed(t *testing.T) {
	// Entries key on the ACCOUNT key with the rail stamped inside.
	p := &Price{Rails: map[string]map[string]string{
		"mobius": {RailKeyRail: "nmi", RailKeyPlanID: "premium_new"},
		"stripe": {RailKeyRail: "stripe", RailKeyStripePriceID: "price_123"},
	}}

	nmi := p.RailAccountConfigs(RailNMI)
	if len(nmi) != 1 || nmi["mobius"][RailKeyPlanID] != "premium_new" {
		t.Fatalf("nmi entries = %v", nmi)
	}
	if cfg := p.GetRailConfig(RailNMI); cfg[RailKeyPlanID] != "premium_new" {
		t.Fatalf("GetRailConfig(nmi) = %v", cfg)
	}
	if cfg := p.GetRailConfig(RailStripe); cfg[RailKeyStripePriceID] != "price_123" {
		t.Fatalf("GetRailConfig(stripe) = %v", cfg)
	}
	if !p.HasRail(RailNMI) || p.HasRail(RailCCBill) {
		t.Fatalf("HasRail: nmi=%v ccbill=%v", p.HasRail(RailNMI), p.HasRail(RailCCBill))
	}

	// Two accounts on one rail: enumeration sees both, the single-entry
	// accessor refuses to guess.
	p.Rails["paykings"] = map[string]string{RailKeyRail: "nmi", RailKeyPlanID: "premium_pk"}
	if got := len(p.RailAccountConfigs(RailNMI)); got != 2 {
		t.Fatalf("expected 2 nmi entries, got %d", got)
	}
	if cfg := p.GetRailConfig(RailNMI); cfg != nil {
		t.Fatalf("ambiguous GetRailConfig should be nil, got %v", cfg)
	}
}
