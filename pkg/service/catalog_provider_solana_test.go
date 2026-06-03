package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// The solana adapter's on-chain paths (PublishPlan / Verify-with-RPC) are covered
// by the decoder test and the devnet suite. These unit tests pin the adapter's own
// branch logic: unconfigured -> pending, attach validation, sync-disabled, and the
// deterministic plan-id derivation.

func TestSolanaAdapter_AutoCreateUnconfiguredIsPending(t *testing.T) {
	a := &solanaAdapter{svc: &Service{}} // no runtime -> SolanaPlanService nil
	days := 30
	_, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID:          uuid.New(),
		Currency:         "USDC",
		UnitAmount:       29_000_000,
		BillingCycleDays: &days,
	})
	if !errors.Is(err, errPendingManualLink) {
		t.Fatalf("unconfigured Solana AutoCreate = %v, want errPendingManualLink", err)
	}
}

func TestSolanaAdapter_AttachRequiresPlanPDA(t *testing.T) {
	a := &solanaAdapter{}
	if _, err := a.Attach(context.Background(), map[string]string{"mint": "x"}); err == nil {
		t.Error("Attach without plan_pda should error")
	}
	got, err := a.Attach(context.Background(), map[string]string{
		solanaKeyPlanPDA:         "PdA111",
		solanaKeyMintSymbol:      "USDC",
		solanaKeyAmountBaseUnits: "29000000",
		"junk":                   "", // empty values are dropped
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got[solanaKeyPlanPDA] != "PdA111" || got[solanaKeyMintSymbol] != "USDC" || got[solanaKeyAmountBaseUnits] != "29000000" {
		t.Errorf("Attach passthrough wrong: %v", got)
	}
	if _, ok := got["junk"]; ok {
		t.Errorf("empty link values should be dropped, got %v", got)
	}
}

func TestSolanaAdapter_VerifySyncDisabledWithoutRPC(t *testing.T) {
	a := &solanaAdapter{svc: &Service{}} // no runtime -> SolanaRPC nil
	drift, missing, err := a.Verify(context.Background(), map[string]string{solanaKeyPlanPDA: "x"}, nil)
	if err != nil || drift != nil || missing {
		t.Fatalf("Verify without RPC = (%v,%v,%v), want sync_disabled (nil,false,nil)", drift, missing, err)
	}
}

func TestSolanaAdapter_UpdateIsNoOp(t *testing.T) {
	a := &solanaAdapter{}
	if err := a.Update(context.Background(), map[string]string{}, mutableUpdate{}); err != nil {
		t.Errorf("Update should be a no-op, got %v", err)
	}
}

func TestSolanaPlanID_Deterministic(t *testing.T) {
	id := uuid.New()
	if solanaPlanID(id) != solanaPlanID(id) {
		t.Error("solanaPlanID must be deterministic for a given price id")
	}
	if solanaPlanID(id) == solanaPlanID(uuid.New()) {
		t.Error("solanaPlanID should differ across distinct price ids")
	}
}
