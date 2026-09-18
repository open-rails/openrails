package models

import (
	"testing"
)

func TestPrice_GetCCBillFlexForm_RequiresFlexID(t *testing.T) {
	price := &Price{PSPLinks: map[string]map[string]string{
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
	p := &Price{PSPLinks: map[string]map[string]string{
		"mobius": {RailKeyRail: "nmi", RailKeyPlanID: "premium_new"},
		"stripe": {RailKeyRail: "stripe", RailKeyStripePriceID: "price_123"},
	}}

	nmi := p.PSPLinksForRail(RailNMI)
	if len(nmi) != 1 || nmi["mobius"][RailKeyPlanID] != "premium_new" {
		t.Fatalf("nmi entries = %v", nmi)
	}
	if cfg := p.PSPLinkForRail(RailNMI); cfg[RailKeyPlanID] != "premium_new" {
		t.Fatalf("PSPLinkForRail(nmi) = %v", cfg)
	}
	if cfg := p.PSPLinkForRail(RailStripe); cfg[RailKeyStripePriceID] != "price_123" {
		t.Fatalf("PSPLinkForRail(stripe) = %v", cfg)
	}
	if !p.HasRail(RailNMI) || p.HasRail(RailCCBill) {
		t.Fatalf("HasRail: nmi=%v ccbill=%v", p.HasRail(RailNMI), p.HasRail(RailCCBill))
	}

	// Two accounts on one rail: enumeration sees both, the single-entry
	// accessor refuses to guess.
	p.PSPLinks["paykings"] = map[string]string{RailKeyRail: "nmi", RailKeyPlanID: "premium_pk"}
	if got := len(p.PSPLinksForRail(RailNMI)); got != 2 {
		t.Fatalf("expected 2 nmi entries, got %d", got)
	}
	if cfg := p.PSPLinkForRail(RailNMI); cfg != nil {
		t.Fatalf("ambiguous PSPLinkForRail should be nil, got %v", cfg)
	}
}
