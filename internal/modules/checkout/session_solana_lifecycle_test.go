package checkout

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestBuildLifecycleState_Cancel(t *testing.T) {
	s := &CheckoutSessionService{}
	subID := uuid.New()
	state := s.buildLifecycleState(&solanaLifecycleState{
		mode:             models.CheckoutSessionModeSolanaCancel,
		subscriptionID:   subID,
		subscriberWallet: "WaLLet1111111111111111111111111111111111111",
		productName:      "Pro",
	})
	if state["flow"] != string(models.CheckoutSessionModeSolanaCancel) {
		t.Errorf("flow = %v, want solana_cancel", state["flow"])
	}
	if state["subscription_id"] != subID.String() {
		t.Errorf("subscription_id = %v, want %s", state["subscription_id"], subID)
	}
	if state["product_name"] != "Pro" {
		t.Errorf("product_name = %v", state["product_name"])
	}
	if _, ok := state["new_price_id"]; ok {
		t.Error("a cancel session must not carry new_price_id")
	}
}

func TestBuildLifecycleState_TierChange(t *testing.T) {
	s := &CheckoutSessionService{}
	subID := uuid.New()
	newPriceID := uuid.New()
	state := s.buildLifecycleState(&solanaLifecycleState{
		mode:           models.CheckoutSessionModeSolanaTierChange,
		subscriptionID: subID,
		tierChange: &resolvedSolanaLifecycleTierChange{
			newPriceID:           newPriceID,
			isUpgrade:            true,
			firstChargeBaseUnits: 31_330_000,
			newTerms: solanaPlanTerms{
				planID:     99,
				mintSymbol: "USDC",
				amount:     50_000_000,
				period:     720,
				createdAt:  1_700_000_000,
			},
		},
	})
	if state["new_price_id"] != newPriceID.String() {
		t.Errorf("new_price_id = %v, want %s", state["new_price_id"], newPriceID)
	}
	if state["tier_is_upgrade"] != true {
		t.Errorf("tier_is_upgrade = %v, want true", state["tier_is_upgrade"])
	}
	if state["tier_new_amount_base_units"] != "50000000" {
		t.Errorf("tier_new_amount_base_units = %v", state["tier_new_amount_base_units"])
	}
	if state["tier_first_charge_base_units"] != "31330000" {
		t.Errorf("tier_first_charge_base_units = %v", state["tier_first_charge_base_units"])
	}

	// Round-trips back through getUint64Field/getBoolField the way the build path reads it.
	if got := getUint64Field(state, "tier_new_amount_base_units"); got != 50_000_000 {
		t.Errorf("read-back amount = %d", got)
	}
	if !getBoolField(state, "tier_is_upgrade") {
		t.Error("read-back upgrade flag must be true")
	}
}
